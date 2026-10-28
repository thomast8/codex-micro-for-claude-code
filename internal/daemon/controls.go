package daemon

import (
	"encoding/json"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustinwdev/codex-micro-for-claude-code/internal/config"
	"github.com/dustinwdev/codex-micro-for-claude-code/internal/keys"
)

const (
	holdFor    = 500 * time.Millisecond // a dial press held this long is "hold"
	stickFire  = 0.8                    // how far the stick goes to act
	stickRearm = 0.3                    // and how near the center to act again
)

// keyControls names the action keys by the switch under them. The mic is one
// wide key on two switches.
var keyControls = map[string]string{
	"ACT06": "fast", "ACT07": "approve", "ACT08": "deny", "ACT09": "branch",
	"ACT10": "mic", "ACT11": "mic", "ACT12": "send",
}

// modifier is the key that, held, makes agent keys assign the current
// session to themselves.
const modifier = "send"

// controls is the pad's controls, mid-gesture.
type controls struct {
	assigned   bool                     // agent keys were pressed while modifier was held
	down       map[string]bool          // switches held down
	holding    map[string]config.Action // hold actions in force, by control
	dialDown   time.Time
	stickArmed bool
	warned     map[string]bool // problems already told of
}

func newControls() controls {
	return controls{down: map[string]bool{}, holding: map[string]config.Action{}, stickArmed: true, warned: map[string]bool{}}
}

// onNotify handles a notification from the pad.
func (d *Daemon) onNotify(method string, params json.RawMessage) {
	switch method {
	case "v.oai.hid":
		var p struct {
			K   string `json:"k"`
			Act int    `json:"act"`
		}
		if json.Unmarshal(params, &p) == nil {
			d.onKey(p.K, p.Act)
		}
	case "v.oai.rad":
		var p struct{ A, D float64 }
		if json.Unmarshal(params, &p) == nil {
			d.onStick(p.A, p.D)
		}
	}
}

// onKey handles a switch: act 1 is down, 0 up, and 2 a dial step.
func (d *Daemon) onKey(key string, act int) {
	switch {
	case strings.HasPrefix(key, "AG"):
		slot, err := strconv.Atoi(key[2:])
		if err != nil || act != 1 {
			return
		}
		d.mu.Lock()
		assigning := d.modifying()
		if assigning {
			d.assigned = true
		}
		d.mu.Unlock()
		if assigning {
			d.assign(slot)
		} else {
			go d.jump(slot)
		}
	case key == "ENC_CW" && act == 2:
		d.control("dial", "right")
	case key == "ENC_CC" && act == 2:
		d.control("dial", "left")
	case key == "ENC_CLK":
		d.mu.Lock()
		held := time.Since(d.dialDown)
		if act == 1 {
			d.dialDown = time.Now()
		}
		pressed := act == 0 && !d.dialDown.IsZero()
		if act == 0 {
			d.dialDown = time.Time{}
		}
		d.mu.Unlock()
		if pressed && held >= holdFor {
			d.control("dial", "hold")
		} else if pressed {
			d.control("dial", "press")
		}
	case keyControls[key] != "":
		d.onSwitch(key, act == 1)
	}
}

// onSwitch handles an action key going down or up. Most act on the way
// down; a hold action is held for as long as the key is. The Codex key acts
// on the way up, unless agent keys were pressed while it was held.
func (d *Daemon) onSwitch(key string, down bool) {
	control := keyControls[key]
	d.mu.Lock()
	was := d.isDown(control)
	if down {
		d.down[key] = true
	} else {
		delete(d.down, key)
	}
	now := d.isDown(control)
	action, holding := d.holding[control]
	fire := false
	switch {
	case now && !was:
		action = d.action("keys", control)
		switch {
		case action.Hold != "":
			d.holding[control] = action
		case control == modifier:
			d.assigned = false
		default:
			fire = true
		}
	case was && !now && holding:
		delete(d.holding, control)
	case was && !now && control == modifier:
		action, fire = d.action("keys", control), !d.assigned
	default:
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	switch {
	case action.Hold != "":
		d.hold(action, now)
	case fire:
		d.log.Println("control keys", control, "->", action)
		go d.run(action)
	}
}

// modifying reports whether the Codex key is held as a modifier: down, and
// not holding a key for a dictation app. The caller holds d.mu.
func (d *Daemon) modifying() bool {
	_, holding := d.holding[modifier]
	return d.isDown(modifier) && !holding
}

// isDown reports whether any switch of a control is down. The caller holds d.mu.
func (d *Daemon) isDown(control string) bool {
	for key := range d.down {
		if keyControls[key] == control {
			return true
		}
	}
	return false
}

func (d *Daemon) hold(action config.Action, down bool) {
	err := d.desktop.Hold(keys.Parse(action.Hold), down)
	if err == nil || !down {
		return
	}
	d.log.Println("hold", action.Hold+":", err)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.warned[err.Error()] {
		d.warned[err.Error()] = true
		d.tell(d.target(), map[string]any{"type": "notice", "text": "Codex Micro: " + err.Error()})
	}
}

// onStick acts once per push past stickFire, and again once the stick has
// come back near the center.
func (d *Daemon) onStick(angle, distance float64) {
	d.mu.Lock()
	fire := false
	switch {
	case distance < stickRearm:
		d.stickArmed = true
	case distance >= stickFire && d.stickArmed:
		d.stickArmed, fire = false, true
	}
	d.mu.Unlock()
	if fire {
		// The angle is a fraction of a turn, clockwise from the right.
		quarter := int(math.Round(angle*4)) % 4
		d.control("stick", []string{"right", "down", "left", "up"}[(quarter+4)%4])
	}
}

func (d *Daemon) control(section, name string) {
	d.mu.Lock()
	action := d.action(section, name)
	d.mu.Unlock()
	d.log.Println("control", section, name, "->", action)
	go d.run(action)
}

// action is what a control does, from config.toml as it is now. The caller
// holds d.mu.
func (d *Daemon) action(section, name string) config.Action {
	d.reload()
	return d.config.Action(section, name)
}

// reload rereads config.toml when it has changed. The caller holds d.mu.
func (d *Daemon) reload() {
	if stamp := config.Modified(d.configPath); stamp != d.configStamp {
		var problems []string
		d.config, problems = config.Load(d.configPath)
		d.configStamp = stamp
		d.log.Println("config reloaded")
		for _, p := range problems {
			d.log.Println("config:", p)
		}
	}
}

// target is the session a control acts on: the focused one, else the one a
// control last went to, else the only one. The caller holds d.mu.
func (d *Daemon) target() *Session {
	for _, s := range []*Session{d.focused, d.last} {
		if d.live(s) {
			return s
		}
	}
	if len(d.sessions) == 1 {
		for _, s := range d.sessions {
			return s
		}
	}
	return nil
}

// tell has a session's plugin carry out a message. The caller holds d.mu.
func (d *Daemon) tell(s *Session, msg map[string]any) {
	if s != nil {
		d.last = s
		d.send(s, msg)
	}
}

// do has the target session's plugin carry out an action inside Claude Code.
func (d *Daemon) do(action string, fields ...any) {
	msg := map[string]any{"type": "action", "action": action}
	for i := 0; i+1 < len(fields); i += 2 {
		msg[fields[i].(string)] = fields[i+1]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tell(d.target(), msg)
}

// run carries out an action.
func (d *Daemon) run(a config.Action) {
	switch {
	case a.Text != "":
		d.do("text", "text", a.Text, "submit", a.Submit)
	case a.Keys != "":
		d.pressTarget(keys.Parse(a.Keys))
	case a.Run != nil:
		cmd := exec.Command(a.Run[0], a.Run[1:]...)
		detach(cmd)
		if err := cmd.Start(); err != nil {
			d.log.Println("run:", err)
			return
		}
		go cmd.Wait()
	case strings.HasPrefix(a.Name, "/"):
		command, args, _ := strings.Cut(a.Name[1:], " ")
		d.do("command", "command", command, "args", args)
	case a.Name == "approve" || a.Name == "deny":
		d.answer(a.Name == "approve")
	case a.Name == "fast" || a.Name == "voice":
		d.do("command", "command", a.Name, "args", "")
	case a.Name == "cycle-mode":
		d.pressTarget(keys.Chord{Mods: []string{"shift"}, Key: "tab"})
	case a.Name == "attention":
		d.mu.Lock()
		s := d.mostUrgent()
		d.mu.Unlock()
		if s != nil {
			d.jump(s.slot)
		}
	case a.Name == "next-session":
		d.cycle(1)
	case a.Name == "prev-session":
		d.cycle(-1)
	case a.Name != "none" && a.Name != "":
		// submit, interrupt, fork, effort-up and effort-down happen inside
		// Claude Code.
		d.do(a.Name)
	}
}

func (d *Daemon) pressTarget(c keys.Chord) {
	d.mu.Lock()
	s := d.target()
	d.mu.Unlock()
	if s != nil {
		d.press(s, c)
	}
}

// mostUrgent is the session that has waited longest on you: for a prompt
// first, then on an error, then with an answer. The caller holds d.mu.
func (d *Daemon) mostUrgent() *Session {
	for _, state := range []string{Awaiting, Error, Unread} {
		var first *Session
		for _, s := range d.sessions {
			if s.state == state && (first == nil || s.since.Before(first.since)) {
				first = s
			}
		}
		if first != nil {
			return first
		}
	}
	return nil
}

// assign moves the current session to a key, swapping with the session
// there.
func (d *Daemon) assign(slot int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.current()
	if s == nil || s.slot == slot || slot < 0 || slot >= slots {
		return
	}
	for _, o := range d.sessions {
		if o.slot == slot {
			o.slot = s.slot
		}
	}
	s.slot = slot
	d.log.Println("session", s.pid, "moved to key", slot+1)
	d.changed()
}

// current is the session you are in: the focused one, else the one whose
// turn you started last. The caller holds d.mu.
func (d *Daemon) current() *Session {
	if d.live(d.focused) {
		return d.focused
	}
	var latest *Session
	for _, s := range d.sessions {
		if latest == nil || s.started.After(latest.started) {
			latest = s
		}
	}
	return latest
}

// jump brings forward the session on a key.
func (d *Daemon) jump(slot int) {
	d.mu.Lock()
	var s *Session
	for _, o := range d.sessions {
		if o.slot == slot && slot >= 0 {
			s = o
		}
	}
	if s != nil {
		d.last = s
	}
	d.mu.Unlock()
	if s != nil {
		d.focus(s)
	}
}

// cycle moves to the next or previous session by key.
func (d *Daemon) cycle(step int) {
	d.mu.Lock()
	var keyed []*Session
	for _, s := range d.sessions {
		if s.slot >= 0 {
			keyed = append(keyed, s)
		}
	}
	if len(keyed) == 0 {
		d.mu.Unlock()
		return
	}
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].slot < keyed[j].slot })
	at := -step
	current := d.target()
	for i, s := range keyed {
		if s == current {
			at = i
		}
	}
	slot := keyed[((at+step)%len(keyed)+len(keyed))%len(keyed)].slot
	d.mu.Unlock()
	d.jump(slot)
}

// answer answers the permission prompt of the session waiting on you: the
// focused one, else the one waiting longest. The session's plugin answers it
// when it waits on the pad; otherwise keys do. With none waiting, deny stops
// the running turn instead.
func (d *Daemon) answer(approve bool) {
	d.mu.Lock()
	s := d.focused
	if !d.live(s) || s.state != Awaiting {
		s = nil
		for _, o := range d.sessions {
			if o.state == Awaiting && (s == nil || o.since.Before(s.since)) {
				s = o
			}
		}
	}
	if s == nil {
		if t := d.target(); !approve && t != nil && t.state == Working {
			d.tell(t, map[string]any{"type": "action", "action": "interrupt"})
		}
		d.mu.Unlock()
		return
	}
	d.last = s
	if s.ask != nil {
		answer := map[bool]string{true: "allow", false: "deny"}[approve]
		d.log.Println("session", s.pid, "prompt:", answer)
		s.ask <- answer
		s.ask = nil
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	if approve {
		d.approve(s)
	} else {
		d.press(s, keys.Chord{Key: "escape"})
	}
}
