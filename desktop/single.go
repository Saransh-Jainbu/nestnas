package main

import "net"

// lockAddr is a loopback port used purely as a cross-platform "already running" lock.
const lockAddr = "127.0.0.1:47831"

// singleInstance returns a channel that fires whenever another launch asks this window to
// come to the front. ok is false if another NestNAS window already exists; in that case it
// has been asked to raise itself and this process should exit.
func singleInstance() (raise <-chan struct{}, ok bool) {
	ln, err := net.Listen("tcp", lockAddr)
	if err != nil {
		if c, err := net.Dial("tcp", lockAddr); err == nil {
			c.Close() // the existing instance treats any connection as "raise yourself"
		}
		return nil, false
	}
	ch := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch, true
}
