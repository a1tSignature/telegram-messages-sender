package wake

/*
#cgo LDFLAGS: -framework AppKit
#include <stdint.h>
void *observe_wake(uintptr_t handle);
void stop_observing_wake(void *observer);
*/
import "C"

import (
	"runtime/cgo"
	"sync"
)

func Observe(notify func()) func() {
	handle := cgo.NewHandle(notify)
	observer := C.observe_wake(C.uintptr_t(handle))
	return sync.OnceFunc(func() {
		C.stop_observing_wake(observer)
		handle.Delete()
	})
}

//export goWorkspaceDidWake
func goWorkspaceDidWake(handle C.uintptr_t) {
	cgo.Handle(handle).Value().(func())()
}
