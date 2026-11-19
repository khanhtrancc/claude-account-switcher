//go:build !windows

package main

// Usage on the taskbar is Windows only.
const taskbarSupported = false

func showTaskbar([]barItem) {}
