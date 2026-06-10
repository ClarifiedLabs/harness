// Package term restores the controlling terminal to a usable state after a
// crashed or misbehaving program leaves it broken — raw mode, echo off,
// mouse or focus reporting enabled — without clearing the screen or
// scrollback (design §10).
package term
