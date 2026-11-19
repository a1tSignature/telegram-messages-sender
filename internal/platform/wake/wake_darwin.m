#import <AppKit/AppKit.h>
#include <stdint.h>

extern void goWorkspaceDidWake(uintptr_t handle);

@interface WorkspaceWakeObserver : NSObject {
@public
    uintptr_t handle;
}
- (void)didWake:(NSNotification *)notification;
@end

@implementation WorkspaceWakeObserver
- (void)didWake:(NSNotification *)notification {
    @synchronized (self) {
        if (handle != 0) {
            goWorkspaceDidWake(handle);
        }
    }
}
@end

void *observe_wake(uintptr_t handle) {
    WorkspaceWakeObserver *observer = [WorkspaceWakeObserver new];
    observer->handle = handle;
    [[[NSWorkspace sharedWorkspace] notificationCenter]
        addObserver:observer selector:@selector(didWake:)
        name:NSWorkspaceDidWakeNotification object:nil];
    return observer;
}

void stop_observing_wake(void *opaque) {
    WorkspaceWakeObserver *observer = opaque;
    @synchronized (observer) {
        observer->handle = 0;
    }
    [[[NSWorkspace sharedWorkspace] notificationCenter]
        removeObserver:observer name:NSWorkspaceDidWakeNotification object:nil];
    [observer release];
}
