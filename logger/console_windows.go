//go:build windows

package main

import "syscall"

// コンソールをUTF-8(65001)にして日本語メッセージの文字化けを防ぐ。
func initConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("SetConsoleOutputCP").Call(65001)
	k.NewProc("SetConsoleCP").Call(65001)
}
