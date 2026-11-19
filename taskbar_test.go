package main

import (
	"image/color"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestBarItems(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	snap := snapshot{Active: "b", Threshold: 96, Accounts: []Account{
		{UUID: "a", Name: "A", Usage: &Usage{Session: 30, Weekly: 50, SessionReset: now.Add(-time.Minute)}},
		{UUID: "b", Name: "B", Usage: &Usage{Session: 80, Weekly: 20}},
		{UUID: "c", Name: "C", Problem: signedOut, Usage: &Usage{Session: 10, Weekly: 10}},
		{UUID: "d", Name: "D"},
	}}

	if got := barItems(snap, taskbarOff, now); got != nil {
		t.Errorf("off: got %v, want nil", got)
	}

	active := barItems(snap, taskbarActive, now)
	if len(active) != 1 || active[0].Session != 80 || active[0].Weekly != 20 || !active[0].Active || active[0].Marked {
		t.Errorf("active: got %+v", active)
	}

	all := barItems(snap, taskbarAll, now)
	if len(all) != 4 {
		t.Fatalf("all: got %d items, want 4", len(all))
	}
	// The active account B comes first, the others keep their order.
	if !all[0].Active || !all[0].Marked || all[0].Session != 80 {
		t.Errorf("first item is not the active account: %+v", all[0])
	}
	// A's five-hour window has reset since it was checked.
	if all[1].Session != 0 || all[1].Weekly != 50 || all[1].Marked {
		t.Errorf("A: got %+v, want 0/50 unmarked", all[1])
	}
	for _, i := range []int{2, 3} {
		if all[i].Session != -1 || all[i].Weekly != -1 {
			t.Errorf("item %d: got %v/%v, want unknown", i, all[i].Session, all[i].Weekly)
		}
	}
	if all[1].Tip == "" {
		t.Error("no tooltip")
	}
}

func TestDrawRings(t *testing.T) {
	const size = 52
	img := drawRings(size, 50, 25, 96)
	c := size / 2
	if a := img.NRGBAAt(c, c).A; a != 0 {
		t.Errorf("center alpha %d, want empty", a)
	}
	// Twelve to six o'clock on the right is filled on the outer ring, the
	// left side is track.
	r := float64(c)
	outer := int(r * (outerIn + outerOut) / 2)
	if p := img.NRGBAAt(c+outer, c); p != (color.NRGBA{colorOK.R, colorOK.G, colorOK.B, 0xFF}) {
		t.Errorf("outer ring at three o'clock: %v, want filled", p)
	}
	if p := img.NRGBAAt(c-outer, c); p.G == colorOK.G && p.A == 255 {
		t.Errorf("outer ring at nine o'clock: %v, want track", p)
	}
	inner := int(r * (innerIn + innerOut) / 2)
	// A quarter of the week: three o'clock is the end, six o'clock is track.
	if p := img.NRGBAAt(c, c+inner); p.A == 255 && p.G == colorOK.G {
		t.Errorf("inner ring at six o'clock: %v, want track", p)
	}
}

// Set TASKBAR_PREVIEW to a number of seconds to see sample accounts on the
// real taskbar, on Windows.
func TestTaskbarPreview(t *testing.T) {
	secs, _ := strconv.Atoi(os.Getenv("TASKBAR_PREVIEW"))
	if secs == 0 || !taskbarSupported {
		t.Skip("TASKBAR_PREVIEW not set")
	}
	now := time.Now()
	snap := snapshot{Active: "spare", Threshold: 96, Accounts: []Account{
		{UUID: "work", Name: "Work", Plan: "Max", Usage: &Usage{Session: 42, Weekly: 18}},
		{UUID: "home", Name: "Personal", Plan: "Pro", Usage: &Usage{Session: 81, Weekly: 64}},
		{UUID: "spare", Name: "Spare", Plan: "Pro", Usage: &Usage{Session: 97, Weekly: 99}},
		{UUID: "old", Name: "Old", Plan: "Pro"},
	}}
	showTaskbar(barItems(snap, taskbarAll, now))
	time.Sleep(time.Duration(secs) * time.Second)
	showTaskbar(nil)
	time.Sleep(500 * time.Millisecond)
}
