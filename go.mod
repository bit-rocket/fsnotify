module github.com/bit-rocket/fsnotify

go 1.17

// github.com/fsnotify/fsnotify v1.7.0
require golang.org/x/sys v0.4.0

retract (
	v1.5.3 // Published an incorrect branch accidentally https://github.com/fsnotify/fsnotify/issues/445
	v1.5.0 // Contains symlink regression https://github.com/fsnotify/fsnotify/pull/394
)
