package airplay

import (
	"errors"
	"sort"
	"strings"
	"time"

	"spoticonn/internal/model"
)

// Target retains the advertised identity and every physical transport record.
type Target struct {
	Device   model.Device
	Members  []model.Device
	Name     string
	Group    bool
	LeaderID string
}

func (t Target) Matches(id string) bool {
	if t.Device.ID == id {
		return true
	}
	for _, d := range t.Members {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (t Target) Ready() error {
	if pod, _, ok := t.HomeTheater(); ok {
		if !pod.Online {
			return errors.New("组合的 HomePod 尚未上线，无法启动音频")
		}
		return nil
	}
	if t.Group && (t.LeaderID == "" || !t.Device.Online) {
		return errors.New("音频组的主设备尚未就绪，请等待主设备上线后重试")
	}
	return nil
}

// HomeTheater deliberately recognizes only an unambiguous native pair. Larger
// groups (including stereo pairs) keep their existing leader routing until their
// member topology has been validated. Names alone never enable staged playback.
func (t Target) HomeTheater() (pod, tv model.Device, ok bool) {
	if !t.Group || len(t.Members) != 2 {
		return
	}
	for _, d := range t.Members {
		switch {
		case strings.HasPrefix(strings.ToLower(d.Model), "audioaccessory"):
			pod = d
		case strings.HasPrefix(strings.ToLower(d.Model), "appletv"):
			tv = d
		}
	}
	ok = pod.ID != "" && tv.ID != ""
	return
}

func (t Target) View(pairings map[string]model.PairingSecret) model.DeviceView {
	v := model.DeviceView{Device: t.Device, Group: t.Group, WaitingForLeader: t.Ready() != nil}
	v.Name = t.Name
	v.TXT = nil
	_, v.Paired = pairings[t.Device.ID]
	pod, tv, staged := t.HomeTheater()
	if staged {
		v.Staged = true
		v.AudioDeviceID, v.JoinDeviceID = pod.ID, tv.ID
		v.Online = pod.Online
		_, v.Paired = pairings[pod.ID]
		v.WaitingForLeader = false
	}
	if t.Group {
		if !staged {
			v.Paired = v.Paired && t.LeaderID != ""
		}
		for _, d := range t.Members {
			_, paired := pairings[d.ID]
			v.Members = append(v.Members, model.DeviceMember{ID: d.ID, Name: displayName(d.Name), Model: d.Model, Online: d.Online, Paired: paired})
		}
	}
	return v
}

func ResolveTarget(devices map[string]model.Device, id string, now time.Time) (Target, bool) {
	for _, t := range Targets(devices, now) {
		if t.Matches(id) {
			return t, true
		}
	}
	return Target{}, false
}

// Targets only merges explicit, nonzero AirPlay group IDs. Room names and
// Bonjour collision suffixes are not evidence that two devices share an output.
func Targets(devices map[string]model.Device, now time.Time) []Target {
	targets := []Target{}
	groups := map[string][]model.Device{}
	for _, d := range devices {
		d.Online = d.Online && now.Sub(d.LastSeen) < 2*time.Minute
		gid := strings.ToLower(strings.TrimSpace(d.TXT["gid"]))
		if gid == "" || strings.Trim(gid, "0-") == "" {
			targets = append(targets, Target{Device: d, Name: displayName(d.Name), Members: []model.Device{d}})
			continue
		}
		groups[gid] = append(groups[gid], d)
	}
	for gid, members := range groups {
		// Standalone Macs also advertise a gid and igl=0, with gcgl=0.
		// A single record needs an advertised discoverable group leader before
		// treating it as a group whose other members have not been found yet.
		if len(members) == 1 && members[0].TXT["gcgl"] != "1" {
			d := members[0]
			targets = append(targets, Target{Device: d, Name: displayName(d.Name), Members: members})
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
		// Prefer one live leader over stale cached advertisements. If there is
		// no live leader, retain a unique cached leader for identity/display only.
		var live, known []model.Device
		for _, d := range members {
			if d.TXT["igl"] == "1" {
				known = append(known, d)
				if d.Online {
					live = append(live, d)
				}
			}
		}
		t := Target{Members: members, Group: true}
		if len(live) == 1 {
			t.Device = live[0]
			t.LeaderID = t.Device.ID
		} else if len(live) == 0 && len(known) == 1 {
			t.Device = known[0]
			t.LeaderID = t.Device.ID
		} else {
			// This placeholder cannot be used as a transport endpoint.
			t.Device = model.Device{ID: "group:" + gid}
		}
		if _, tv, ok := t.HomeTheater(); ok {
			// Keep a stable display identity even if the TV is offline or the
			// native leader changes. This is not the initial audio endpoint.
			t.Device = tv
		}
		t.Name = t.Device.TXT["gpn"] // TXT values are plain text, not DNS-escaped labels.
		if t.Name == "" {
			for _, d := range members {
				if t.Name = d.TXT["gpn"]; t.Name != "" {
					break
				}
			}
		}
		if t.Name == "" {
			if t.LeaderID != "" {
				t.Name = displayName(t.Device.Name)
			} else {
				t.Name = displayName(members[0].Name)
			}
		}
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Name == targets[j].Name {
			return targets[i].Device.ID < targets[j].Device.ID
		}
		return targets[i].Name < targets[j].Name
	})
	return targets
}

// DNS presentation format uses both escaped characters (\ space, \() and
// decimal byte escapes (\032, including UTF-8 bytes). Do not unescape twice:
// a literal backslash followed by digits must stay literal after decoding.
func displayName(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if i+3 < len(s) && decimal(s[i+1]) && decimal(s[i+2]) && decimal(s[i+3]) {
				n := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
				if n <= 255 {
					out.WriteByte(byte(n))
					i += 3
					continue
				}
				out.WriteByte(s[i])
				continue
			}
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func decimal(b byte) bool { return b >= '0' && b <= '9' }
