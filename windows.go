// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows
// +build windows

package fsnotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Watcher watches a set of files, delivering events to a channel.
type Watcher struct {
	Events chan Event
	Errors chan error

	port  syscall.Handle // Handle to completion port
	input chan *input    // Inputs to the reader are sent on this channel
	quit  chan chan<- error

	mu       sync.Mutex // Protects access to watches, isClosed
	watches  watchMap   // Map of watches (key: i-number)
	isClosed bool       // Set to true when Close() is first called
}

// NewWatcher establishes a new watcher with the underlying OS and begins waiting for events.
func NewWatcher() (*Watcher, error) {
	port, err := syscall.CreateIoCompletionPort(syscall.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	w := &Watcher{
		port:    port,
		watches: make(watchMap),
		input:   make(chan *input, 1),
		Events:  make(chan Event, 50),
		Errors:  make(chan error),
		quit:    make(chan chan<- error, 1),
	}
	go w.readEvents()
	return w, nil
}

// Close removes all watches and closes the events channel.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.isClosed {
		w.mu.Unlock()
		return nil
	}
	w.isClosed = true
	w.mu.Unlock()

	// Send "quit" message to the reader goroutine
	ch := make(chan error)
	w.quit <- ch
	if err := w.wakeupReader(); err != nil {
		return err
	}
	return <-ch
}

// Add starts watching the named file or directory (non-recursively).
func (w *Watcher) Add(name string) error {
	w.mu.Lock()
	if w.isClosed {
		w.mu.Unlock()
		return errors.New("watcher already closed")
	}
	w.mu.Unlock()

	in := &input{
		op:    opAddWatch,
		path:  filepath.Clean(name),
		flags: sysFSALLEVENTS,
		reply: make(chan error),
	}
	w.input <- in
	if err := w.wakeupReader(); err != nil {
		return err
	}
	return <-in.reply
}

// Remove stops watching the the named file or directory (non-recursively).
func (w *Watcher) Remove(name string) error {
	in := &input{
		op:    opRemoveWatch,
		path:  filepath.Clean(name),
		reply: make(chan error),
	}
	w.input <- in
	if err := w.wakeupReader(); err != nil {
		return err
	}
	return <-in.reply
}

// WatchList returns the directories and files that are being monitered.
func (w *Watcher) WatchList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	entries := make([]string, 0, len(w.watches))
	for _, entry := range w.watches {
		for _, watchEntry := range entry {
			entries = append(entries, watchEntry.path)
		}
	}

	return entries
}

const (
	// Options for AddWatch
	sysFSONESHOT = 0x80000000
	sysFSONLYDIR = 0x1000000

	// Events
	sysFSACCESS     = 0x1
	sysFSALLEVENTS  = 0xfff
	sysFSATTRIB     = 0x4
	sysFSCLOSE      = 0x18
	sysFSCREATE     = 0x100
	sysFSDELETE     = 0x200
	sysFSDELETESELF = 0x400
	sysFSMODIFY     = 0x2
	sysFSMOVE       = 0xc0
	sysFSMOVEDFROM  = 0x40
	sysFSMOVEDTO    = 0x80
	sysFSMOVESELF   = 0x800

	// Special events
	sysFSIGNORED   = 0x8000
	sysFSQOVERFLOW = 0x4000
)

func newEvent(name string, mask uint32) Event {
	e := Event{Name: name}
	if mask&sysFSCREATE == sysFSCREATE || mask&sysFSMOVEDTO == sysFSMOVEDTO {
		e.Op |= Create
	}
	if mask&sysFSDELETE == sysFSDELETE || mask&sysFSDELETESELF == sysFSDELETESELF {
		e.Op |= Remove
	}
	if mask&sysFSMODIFY == sysFSMODIFY {
		e.Op |= Write
	}
	if mask&sysFSMOVE == sysFSMOVE || mask&sysFSMOVESELF == sysFSMOVESELF || mask&sysFSMOVEDFROM == sysFSMOVEDFROM {
		e.Op |= Rename
	}
	if mask&sysFSATTRIB == sysFSATTRIB {
		e.Op |= Chmod
	}
	return e
}

const (
	opAddWatch = iota
	opRemoveWatch
)

const (
	provisional uint64 = 1 << (32 + iota)
)

type input struct {
	op    int
	path  string
	flags uint32
	reply chan error
}

type inode struct {
	handle syscall.Handle
	volume uint32
	index  uint64
}

type watch struct {
	ov     syscall.Overlapped
	ino    *inode            // i-number
	path   string            // Directory path
	mask   uint64            // Directory itself is being watched with these notify flags
	names  map[string]uint64 // Map of names being watched and their notify flags
	rename string            // Remembers the old name while renaming a file
	buf    [65536]byte       // 64K buffer
}

type (
	indexMap map[uint64]*watch
	watchMap map[uint32]indexMap
)

func (w *Watcher) wakeupReader() error {
	err := syscall.PostQueuedCompletionStatus(w.port, 0, 0, nil)
	if err != nil {
		return os.NewSyscallError("PostQueuedCompletionStatus", err)
	}
	return nil
}

func getDir(pathname string) (dir string, err error) {
	attr, err := syscall.GetFileAttributes(syscall.StringToUTF16Ptr(pathname))
	if err != nil {
		return "", os.NewSyscallError("GetFileAttributes", err)
	}
	if attr&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		dir = pathname
	} else {
		dir, _ = filepath.Split(pathname)
		dir = filepath.Clean(dir)
	}
	return
}

func getIno(path string) (ino *inode, err error) {
	h, err := syscall.CreateFile(syscall.StringToUTF16Ptr(path),
		syscall.FILE_LIST_DIRECTORY,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, os.NewSyscallError("CreateFile", err)
	}

	var fi syscall.ByHandleFileInformation
	err = syscall.GetFileInformationByHandle(h, &fi)
	if err != nil {
		syscall.CloseHandle(h)
		return nil, os.NewSyscallError("GetFileInformationByHandle", err)
	}
	ino = &inode{
		handle: h,
		volume: fi.VolumeSerialNumber,
		index:  uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow),
	}
	return ino, nil
}

// Must run within the I/O thread.
func (m watchMap) get(ino *inode) *watch {
	if i := m[ino.volume]; i != nil {
		return i[ino.index]
	}
	return nil
}

// Must run within the I/O thread.
func (m watchMap) set(ino *inode, watch *watch) {
	i := m[ino.volume]
	if i == nil {
		i = make(indexMap)
		m[ino.volume] = i
	}
	i[ino.index] = watch
}

// Must run within the I/O thread.
func (w *Watcher) addWatch(pathname string, flags uint64) error {
	dir, err := getDir(pathname)
	if err != nil {
		return err
	}
	if flags&sysFSONLYDIR != 0 && pathname != dir {
		return nil
	}

	ino, err := getIno(dir)
	if err != nil {
		return err
	}
	w.mu.Lock()
	watchEntry := w.watches.get(ino)
	w.mu.Unlock()
	if watchEntry == nil {
		_, err := syscall.CreateIoCompletionPort(ino.handle, w.port, 0, 0)
		if err != nil {
			syscall.CloseHandle(ino.handle)
			return os.NewSyscallError("CreateIoCompletionPort", err)
		}
		watchEntry = &watch{
			ino:   ino,
			path:  dir,
			names: make(map[string]uint64),
		}
		w.mu.Lock()
		w.watches.set(ino, watchEntry)
		w.mu.Unlock()
		flags |= provisional
	} else {
		syscall.CloseHandle(ino.handle)
	}
	if pathname == dir {
		watchEntry.mask |= flags
	} else {
		watchEntry.names[filepath.Base(pathname)] |= flags
	}

	err = w.startRead(watchEntry)
	if err != nil {
		return err
	}

	if pathname == dir {
		watchEntry.mask &= ^provisional
	} else {
		watchEntry.names[filepath.Base(pathname)] &= ^provisional
	}
	return nil
}

// Must run within the I/O thread.
func (w *Watcher) remWatch(pathname string) error {
	dir, err := getDir(pathname)
	if err != nil {
		return err
	}
	ino, err := getIno(dir)
	if err != nil {
		return err
	}

	w.mu.Lock()
	watch := w.watches.get(ino)
	w.mu.Unlock()

	err = syscall.CloseHandle(ino.handle)
	if err != nil {
		w.Errors <- os.NewSyscallError("CloseHandle", err)
	}
	if watch == nil {
		return fmt.Errorf("%w: %s", ErrNonExistentWatch, pathname)
	}
	if pathname == dir {
		w.sendEvent(watch.path, watch.mask&sysFSIGNORED)
		watch.mask = 0
	} else {
		name := filepath.Base(pathname)
		w.sendEvent(filepath.Join(watch.path, name), watch.names[name]&sysFSIGNORED)
		delete(watch.names, name)
	}
	return w.startRead(watch)
}

// Must run within the I/O thread.
func (w *Watcher) deleteWatch(watch *watch) {
	for name, mask := range watch.names {
		if mask&provisional == 0 {
			w.sendEvent(filepath.Join(watch.path, name), mask&sysFSIGNORED)
		}
		delete(watch.names, name)
	}
	if watch.mask != 0 {
		if watch.mask&provisional == 0 {
			w.sendEvent(watch.path, watch.mask&sysFSIGNORED)
		}
		watch.mask = 0
	}
}

// Must run within the I/O thread.
func (w *Watcher) startRead(watch *watch) error {
	err := syscall.CancelIo(watch.ino.handle)
	if err != nil {
		w.Errors <- os.NewSyscallError("CancelIo", err)
		w.deleteWatch(watch)
	}
	mask := toWindowsFlags(watch.mask)
	for _, m := range watch.names {
		mask |= toWindowsFlags(m)
	}
	if mask == 0 {
		err := syscall.CloseHandle(watch.ino.handle)
		if err != nil {
			w.Errors <- os.NewSyscallError("CloseHandle", err)
		}
		w.mu.Lock()
		delete(w.watches[watch.ino.volume], watch.ino.index)
		w.mu.Unlock()
		return nil
	}

	rdErr := syscall.ReadDirectoryChanges(watch.ino.handle, &watch.buf[0],
		uint32(unsafe.Sizeof(watch.buf)), false, mask, nil, &watch.ov, 0)
	if rdErr != nil {
		err := os.NewSyscallError("ReadDirectoryChanges", rdErr)
		if rdErr == syscall.ERROR_ACCESS_DENIED && watch.mask&provisional == 0 {
			// Watched directory was probably removed
			if w.sendEvent(watch.path, watch.mask&sysFSDELETESELF) {
				if watch.mask&sysFSONESHOT != 0 {
					watch.mask = 0
				}
			}
			err = nil
		}
		w.deleteWatch(watch)
		w.startRead(watch)
		return err
	}
	return nil
}

// readEvents reads from the I/O completion port, converts the
// received events into Event objects and sends them via the Events channel.
// Entry point to the I/O thread.
func (w *Watcher) readEvents() {
	var (
		n, key uint32
		ov     *syscall.Overlapped
	)
	runtime.LockOSThread()

	for {
		qErr := syscall.GetQueuedCompletionStatus(w.port, &n, &key, &ov, syscall.INFINITE)
		// This error is handled after the watch == nil check below. NOTE: this
		// seems odd, note sure if it's correct.

		watch := (*watch)(unsafe.Pointer(ov))
		if watch == nil {
			select {
			case ch := <-w.quit:
				w.mu.Lock()
				var indexes []indexMap
				for _, index := range w.watches {
					indexes = append(indexes, index)
				}
				w.mu.Unlock()
				for _, index := range indexes {
					for _, watch := range index {
						w.deleteWatch(watch)
						w.startRead(watch)
					}
				}

				err := syscall.CloseHandle(w.port)
				if err != nil {
					err = os.NewSyscallError("CloseHandle", err)
				}
				close(w.Events)
				close(w.Errors)
				ch <- err
				return
			case in := <-w.input:
				switch in.op {
				case opAddWatch:
					in.reply <- w.addWatch(in.path, uint64(in.flags))
				case opRemoveWatch:
					in.reply <- w.remWatch(in.path)
				}
			default:
			}
			continue
		}

		switch qErr {
		case syscall.ERROR_MORE_DATA:
			if watch == nil {
				w.Errors <- errors.New("ERROR_MORE_DATA has unexpectedly null lpOverlapped buffer")
			} else {
				// The i/o succeeded but the buffer is full.
				// In theory we should be building up a full packet.
				// In practice we can get away with just carrying on.
				n = uint32(unsafe.Sizeof(watch.buf))
			}
		case syscall.ERROR_ACCESS_DENIED:
			// Watched directory was probably removed
			w.sendEvent(watch.path, watch.mask&sysFSDELETESELF)
			w.deleteWatch(watch)
			w.startRead(watch)
			continue
		case syscall.ERROR_OPERATION_ABORTED:
			// CancelIo was called on this handle
			continue
		default:
			w.Errors <- os.NewSyscallError("GetQueuedCompletionPort", qErr)
			continue
		case nil:
		}

		var offset uint32
		for {
			if n == 0 {
				w.Events <- newEvent("", sysFSQOVERFLOW)
				w.Errors <- errors.New("short read in readEvents()")
				break
			}

			// Point "raw" to the event in the buffer
			raw := (*syscall.FileNotifyInformation)(unsafe.Pointer(&watch.buf[offset]))
			// TODO: Consider using unsafe.Slice that is available from go1.17
			// https://stackoverflow.com/questions/51187973/how-to-create-an-array-or-a-slice-from-an-array-unsafe-pointer-in-golang
			// instead of using a fixed syscall.MAX_PATH buf, we create a buf that is the size of the path name
			size := int(raw.FileNameLength / 2)
			var buf []uint16
			sh := (*reflect.SliceHeader)(unsafe.Pointer(&buf))
			sh.Data = uintptr(unsafe.Pointer(&raw.FileName))
			sh.Len = size
			sh.Cap = size
			name := syscall.UTF16ToString(buf)
			fullname := filepath.Join(watch.path, name)

			var mask uint64
			switch raw.Action {
			case syscall.FILE_ACTION_REMOVED:
				mask = sysFSDELETESELF
			case syscall.FILE_ACTION_MODIFIED:
				mask = sysFSMODIFY
			case syscall.FILE_ACTION_RENAMED_OLD_NAME:
				watch.rename = name
			case syscall.FILE_ACTION_RENAMED_NEW_NAME:
				// Update saved path of all sub-watches.
				old := filepath.Join(watch.path, watch.rename)
				w.mu.Lock()
				for _, watchMap := range w.watches {
					for _, ww := range watchMap {
						if strings.HasPrefix(ww.path, old) {
							ww.path = filepath.Join(fullname, strings.TrimPrefix(ww.path, old))
						}
					}
				}
				w.mu.Unlock()

				if watch.names[watch.rename] != 0 {
					watch.names[name] |= watch.names[watch.rename]
					delete(watch.names, watch.rename)
					mask = sysFSMOVESELF
				}
			}

			sendNameEvent := func() {
				if w.sendEvent(fullname, watch.names[name]&mask) {
					if watch.names[name]&sysFSONESHOT != 0 {
						delete(watch.names, name)
					}
				}
			}
			if raw.Action != syscall.FILE_ACTION_RENAMED_NEW_NAME {
				sendNameEvent()
			}
			if raw.Action == syscall.FILE_ACTION_REMOVED {
				w.sendEvent(fullname, watch.names[name]&sysFSIGNORED)
				delete(watch.names, name)
			}
			if w.sendEvent(fullname, watch.mask&toFSnotifyFlags(raw.Action)) {
				if watch.mask&sysFSONESHOT != 0 {
					watch.mask = 0
				}
			}
			if raw.Action == syscall.FILE_ACTION_RENAMED_NEW_NAME {
				fullname = filepath.Join(watch.path, watch.rename)
				sendNameEvent()
			}

			// Move to the next event in the buffer
			if raw.NextEntryOffset == 0 {
				break
			}
			offset += raw.NextEntryOffset

			// Error!
			if offset >= n {
				w.Errors <- errors.New("Windows system assumed buffer larger than it is, events have likely been missed.")
				break
			}
		}

		if err := w.startRead(watch); err != nil {
			w.Errors <- err
		}
	}
}

func (w *Watcher) sendEvent(name string, mask uint64) bool {
	if mask == 0 {
		return false
	}
	event := newEvent(name, uint32(mask))
	select {
	case ch := <-w.quit:
		w.quit <- ch
	case w.Events <- event:
	}
	return true
}

func toWindowsFlags(mask uint64) uint32 {
	var m uint32
	if mask&sysFSACCESS != 0 {
		m |= syscall.FILE_NOTIFY_CHANGE_LAST_ACCESS
	}
	if mask&sysFSMODIFY != 0 {
		m |= syscall.FILE_NOTIFY_CHANGE_LAST_WRITE
	}
	if mask&sysFSATTRIB != 0 {
		m |= syscall.FILE_NOTIFY_CHANGE_ATTRIBUTES
	}
	if mask&(sysFSMOVE|sysFSCREATE|sysFSDELETE) != 0 {
		m |= syscall.FILE_NOTIFY_CHANGE_FILE_NAME | syscall.FILE_NOTIFY_CHANGE_DIR_NAME
	}
	return m
}

func toFSnotifyFlags(action uint32) uint64 {
	switch action {
	case syscall.FILE_ACTION_ADDED:
		return sysFSCREATE
	case syscall.FILE_ACTION_REMOVED:
		return sysFSDELETE
	case syscall.FILE_ACTION_MODIFIED:
		return sysFSMODIFY
	case syscall.FILE_ACTION_RENAMED_OLD_NAME:
		return sysFSMOVEDFROM
	case syscall.FILE_ACTION_RENAMED_NEW_NAME:
		return sysFSMOVEDTO
	}
	return 0
}
