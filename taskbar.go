package main

import (
	"image"
	"image/color"
	"math"
	"slices"
	"time"
)

// On Windows the switcher can show usage on the taskbar, next to the
// notification area: per account two nested rings, the outer one for the
// five-hour limit and the inner one for the week, with both percentages
// to their right.

// Values of State.Taskbar.
const (
	taskbarOff    = ""
	taskbarActive = "active"
	taskbarAll    = "all"
)

// barItem is one account on the taskbar. A percentage below zero is unknown.
type barItem struct {
	Session, Weekly float64
	Threshold       float64
	Active          bool
	// Marked is set on the active account when several are shown.
	Marked bool
	Tip    string
}

// barItems lists what the taskbar shows for mode, nil when it is off.
func barItems(snap snapshot, mode string, now time.Time) []barItem {
	if mode != taskbarActive && mode != taskbarAll {
		return nil
	}
	var items []barItem
	for _, a := range snap.Accounts {
		active := a.UUID == snap.Active
		if mode == taskbarActive && !active {
			continue
		}
		it := barItem{Session: -1, Weekly: -1, Threshold: snap.Threshold, Active: active,
			Tip: a.label() + "\n" + usageText(a, snap.Threshold, now)}
		if a.Usage != nil && a.Problem == "" {
			u := a.Usage.current(now)
			it.Session, it.Weekly = u.Session, u.Weekly
		}
		items = append(items, it)
	}
	if mode == taskbarAll && len(items) > 1 {
		for i := range items {
			items[i].Marked = items[i].Active
		}
		// The active account always comes first, at the left.
		slices.SortStableFunc(items, func(a, b barItem) int {
			switch {
			case a.Active == b.Active:
				return 0
			case a.Active:
				return -1
			}
			return 1
		})
	}
	return items
}

// Ring geometry as fractions of the icon's radius.
const (
	outerIn, outerOut = 0.74, 1.0
	innerIn, innerOut = 0.40, 0.64
)

// drawRings draws the two nested rings at size×size: the outer one fills
// with the five-hour usage, the inner one with the week's.
func drawRings(size int, session, weekly, threshold float64) *image.NRGBA {
	const samples = 4
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	c := float64(size) / 2
	ring := func(pct float64) (color.RGBA, float64) {
		if pct < 0 {
			return colorUnknown, 0
		}
		return usageColor(pct, threshold), math.Min(pct, 100) / 100
	}
	sFill, sFrac := ring(session)
	wFill, wFrac := ring(weekly)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples - c
					py := float64(y) + (float64(sy)+0.5)/samples - c
					d := math.Hypot(px, py) / c
					var fill color.RGBA
					var frac float64
					switch {
					case d >= outerIn && d <= outerOut:
						fill, frac = sFill, sFrac
					case d >= innerIn && d <= innerOut:
						fill, frac = wFill, wFrac
					default:
						continue
					}
					// Clockwise from twelve o'clock.
					angle := math.Mod(math.Atan2(px, -py)+2*math.Pi, 2*math.Pi) / (2 * math.Pi)
					col := colorTrack
					if frac > 0 && angle <= frac {
						col = fill
					}
					w := float64(col.A) / 255
					r, g, b, a = r+float64(col.R)*w, g+float64(col.G)*w, b+float64(col.B)*w, a+w
				}
			}
			if a == 0 {
				continue
			}
			n := float64(samples * samples)
			img.SetNRGBA(x, y, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a / n * 255)})
		}
	}
	return img
}
