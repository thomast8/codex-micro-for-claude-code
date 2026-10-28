package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustinwdev/codex-micro-for-claude-code/internal/pad"
	"github.com/dustinwdev/codex-micro-for-claude-code/internal/proc"
)

const (
	heartbeat  = 30 * time.Second // the pad forgets status colors after a quiet spell
	pollEvery  = 60 * time.Second // battery and charging
	scanEvery  = 2 * time.Second  // looking for the pad while it is away
	reapEvery  = 3 * time.Second
	callFor    = 3 * time.Second
	retryAfter = time.Second
)

// The ChatGPT app's palette, so the pad looks the same in either.
const (
	effectOff    = 0
	effectSolid  = 1
	effectBreath = 4
)

type look struct {
	color  int
	effect int
}

var looks = map[string]look{
	Idle:     {0xFFFFFF, effectSolid},
	Working:  {0x304FFE, effectSolid},
	Unread:   {0x00FF4C, effectSolid},
	Awaiting: {0xFF6D00, effectBreath},
	Error:    {0xFF0033, effectSolid},
}

// appName names an app bundle the way System Settings lists it.
func appName(app string) string {
	if app == "" {
		return "your terminal app"
	}
	return strings.TrimSuffix(filepath.Base(app), ".app")
}

// deviceLoop keeps the pad open, and its battery known.
func (d *Daemon) deviceLoop(ctx context.Context) {
	for {
		d.mu.Lock()
		device := d.device
		d.mu.Unlock()
		wait := scanEvery
		if device == nil {
			device = d.open()
		}
		var lost <-chan struct{}
		if device != nil {
			d.pollStatus(device)
			wait, lost = pollEvery, device.Lost()
		}
		select {
		case <-ctx.Done():
			return
		case <-lost:
		case <-time.After(wait):
		}
	}
}

func (d *Daemon) open() *pad.Device {
	d.mu.Lock()
	d.reload()
	channel := d.config.Channel
	d.mu.Unlock()
	chosen, ok := pad.Pick(pad.Find(), channel)
	if !ok {
		return nil
	}
	device, err := pad.Open(chosen, d.onNotify)
	var firmware struct {
		Version string `json:"version"`
	}
	if err == nil {
		var result json.RawMessage
		if result, err = device.Call("sys.version", nil, callFor); err == nil {
			err = json.Unmarshal(result, &firmware)
		}
		if err != nil {
			device.Close()
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.notAllowed = errors.Is(err, pad.ErrNotPermitted)
	if d.notAllowed {
		d.makeWay()
		err = fmt.Errorf("macOS blocked access: allow %s under System Settings > "+
			"Privacy & Security > Input Monitoring, then reopen it", appName(d.origin.App))
	}
	if err != nil {
		if d.info.Problem != err.Error() {
			d.log.Println("open failed:", err)
			d.info = DeviceInfo{Problem: err.Error()}
			d.changed()
		}
		return nil
	}
	d.device = device
	d.info = DeviceInfo{Connected: true, Model: device.Model, Transport: device.Transport, Firmware: firmware.Version}
	d.log.Println("device", device.Model, device.Transport, firmware.Version)
	d.changed()
	go func() {
		<-device.Lost()
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.device == device {
			d.log.Println("device lost")
			d.device = nil
			d.info = DeviceInfo{}
			d.changed()
		}
	}()
	return device
}

func (d *Daemon) pollStatus(device *pad.Device) {
	result, err := device.Call("device.status", nil, callFor)
	var st struct {
		Battery    *int `json:"battery"`
		IsCharging bool `json:"is_charging"`
	}
	if err != nil || json.Unmarshal(result, &st) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.device != device {
		return
	}
	info := d.info
	info.Battery, info.Charging = st.Battery, st.IsCharging
	if !sameInfo(info, d.info) {
		d.info = info
		d.changed()
	}
}

func sameInfo(a, b DeviceInfo) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// lightsLoop sends the pad each key whose look changed, and every key on the
// heartbeat. Over Bluetooth a report can take a third of a second while the
// link idles, so a change sends only its own key.
func (d *Daemon) lightsLoop(ctx context.Context) {
	lit := map[int]look{} // what the pad shows now
	var shown *pad.Device
	failures := 0
	beat := time.NewTimer(heartbeat)
	for {
		everything := false
		select {
		case <-ctx.Done():
			return
		case <-d.dirty:
		case <-beat.C:
			everything = true
		}
		if everything {
			beat.Reset(heartbeat)
		}

		d.mu.Lock()
		device := d.device
		want := map[int]look{}
		for i := range slots {
			want[i] = look{0, effectOff}
		}
		for _, s := range d.sessions {
			if s.slot >= 0 {
				want[s.slot] = looks[s.state]
			}
		}
		d.mu.Unlock()
		if device == nil {
			continue
		}
		if device != shown {
			lit, shown = map[int]look{}, device
		}

		type light struct {
			ID         int      `json:"id"`
			Color      int      `json:"color"`
			Brightness int      `json:"brightness"`
			Effect     int      `json:"effect"`
			Speed      *float64 `json:"speed,omitempty"`
		}
		var params []light
		for i := range slots {
			l := want[i]
			if old, ok := lit[i]; ok && old == l && !everything {
				continue
			}
			p := light{ID: i, Color: l.color, Effect: l.effect}
			if l.color != 0 {
				p.Brightness = 1
			}
			if l.effect == effectBreath {
				speed := 0.5
				p.Speed = &speed
			}
			params = append(params, p)
		}
		if len(params) == 0 {
			continue
		}
		if _, err := device.Call("v.oai.thstatus", params, callFor); err != nil {
			// Often a link still settling: try again shortly, then less often.
			if failures++; failures <= 3 || failures%10 == 0 {
				d.log.Printf("lights failed (%d): %v", failures, err)
			}
			lit = map[int]look{}
			time.AfterFunc(min(retryAfter*time.Duration(failures), heartbeat), d.relight)
			continue
		}
		failures = 0
		for _, p := range params {
			lit[p.ID] = want[p.ID]
		}
	}
}

// relight has the lights sent again.
func (d *Daemon) relight() {
	select {
	case d.dirty <- struct{}{}:
	default:
	}
}

// reapLoop ends any session whose process is gone, in case its attach
// hangs instead of closing the connection.
func (d *Daemon) reapLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reapEvery):
		}
		d.mu.Lock()
		for _, s := range d.sessions {
			if !proc.Alive(s.pid) {
				d.log.Println("reap", s.pid)
				s.close()
			}
		}
		d.mu.Unlock()
	}
}
