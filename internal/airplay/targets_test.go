package airplay

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"spoticonn/internal/model"
)

func livingRoom(t *testing.T) map[string]model.Device {
	t.Helper()
	devices := map[string]model.Device{}
	for _, record := range []struct{ id, name, model, leader, address string }{
		{"06:03:16:5E:17:B1", `Living\ Room`, "AppleTV14,1", "1", "192.0.2.10"},
		{"6A:9C:DD:77:21:C7", `Living\ Room\ \(2\)`, "AudioAccessory6,1", "0", "192.0.2.11"},
	} {
		e := &zeroconf.ServiceEntry{ServiceRecord: zeroconf.ServiceRecord{Instance: record.name}, Port: 7000, TTL: 120, AddrIPv4: []net.IP{net.ParseIP(record.address)}, Text: []string{
			"deviceid=" + record.id, "model=" + record.model, "igl=" + record.leader, "gcgl=1", "gpn=Living Room", "gid=FA6CE6B6-16B6-557C-B2D2-451F3C68D932", "pk=private-transport-data",
		}}
		d, ok := DeviceFromEntry(e)
		if !ok {
			t.Fatal("discovery rejected fixture")
		}
		devices[d.ID] = d
	}
	return devices
}

func TestAppleTVHomePodGroup(t *testing.T) {
	devices := livingRoom(t)
	before, _ := json.Marshal(devices)
	targets := Targets(devices, time.Now())
	if len(targets) != 1 {
		t.Fatalf("expected one output, got %d", len(targets))
	}
	target := targets[0]
	if target.Name != "Living Room" || target.Device.ID != "0603165e17b1" || target.Device.Address != "192.0.2.10" || target.Ready() != nil {
		t.Fatalf("incorrect group endpoint: %+v", target)
	}
	for id := range devices {
		resolved, ok := ResolveTarget(devices, id, time.Now())
		if !ok || resolved.Device.ID != target.Device.ID {
			t.Fatalf("old member selection %s no longer resolves", id)
		}
	}
	view := target.View(map[string]model.PairingSecret{"6a9cdd7721c7": {Credentials: "homepod-key"}})
	if view.Paired || !view.Group || !view.Online || len(view.Members) != 2 || view.Members[1].Name != "Living Room (2)" {
		t.Fatalf("wrong display or credentials: %+v", view)
	}
	b, _ := json.Marshal(view)
	if strings.Contains(string(b), "private-transport-data") || strings.Contains(string(b), "homepod-key") {
		t.Fatal("public view leaked private data")
	}
	after, _ := json.Marshal(devices)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("building views modified stored transport records")
	}
}

func TestGroupRequiresUnambiguousLiveLeader(t *testing.T) {
	for _, scenario := range []string{"missing", "offline", "expired", "conflicting", "leadership changed"} {
		t.Run(scenario, func(t *testing.T) {
			devices := livingRoom(t)
			tv := devices["0603165e17b1"]
			switch scenario {
			case "missing":
				delete(devices, tv.ID)
			case "offline":
				tv.Online = false
				devices[tv.ID] = tv
			case "expired", "leadership changed":
				tv.LastSeen = time.Now().Add(-3 * time.Minute)
				devices[tv.ID] = tv
			}
			if scenario == "conflicting" || scenario == "leadership changed" {
				devices["6a9cdd7721c7"].TXT["igl"] = "1"
			}
			targets := Targets(devices, time.Now())
			if len(targets) != 1 {
				t.Fatal("group split while its leader changed")
			}
			target := targets[0]
			if scenario == "leadership changed" {
				if target.Ready() != nil || target.Device.ID != "6a9cdd7721c7" {
					t.Fatal("stale leader overrode the current live leader")
				}
			} else if target.Ready() == nil || target.View(nil).Online || !target.View(nil).WaitingForLeader {
				t.Fatal("group was connectable without a unique live leader")
			}
		})
	}
}

func TestNamesNeverDetermineGroupMembership(t *testing.T) {
	for _, gid := range []string{"", "00000000-0000-0000-0000-000000000000"} {
		devices := livingRoom(t)
		for _, d := range devices {
			d.TXT["gid"] = gid
		}
		targets := Targets(devices, time.Now())
		if len(targets) != 2 || targets[0].Group || targets[1].Group || targets[1].Name != "Living Room (2)" {
			t.Fatalf("unrelated same-room devices were merged: %+v", targets)
		}
	}
	devices := livingRoom(t)
	devices["6a9cdd7721c7"].TXT["gid"] = "another-group"
	if len(Targets(devices, time.Now())) != 2 {
		t.Fatal("different group IDs were merged")
	}
}

func TestStandaloneMacWithGroupIDRemainsSelectable(t *testing.T) {
	d := model.Device{ID: "mac", Name: "s7mac", Model: "MacBookPro18,3", Online: true, LastSeen: time.Now(), TXT: map[string]string{
		"gid": "44AD20A9-45F2-4600-B23D-20ECCA41A890", "igl": "0", "gcgl": "0",
	}}
	targets := Targets(map[string]model.Device{d.ID: d}, time.Now())
	if len(targets) != 1 || targets[0].Group || targets[0].Device.ID != d.ID || !targets[0].View(nil).Online || targets[0].Ready() != nil {
		t.Fatalf("standalone Mac became an unavailable group: %+v", targets)
	}
}

func TestDNSNameDecodedExactlyOnce(t *testing.T) {
	for input, want := range map[string]string{
		`Living\ Room\ \(2\)`:         "Living Room (2)",
		`Living\032Room\032\0402\041`: "Living Room (2)",
		`\229\174\162\229\142\133`:    "客厅",
		`Desk\\032Speaker`:            `Desk\032Speaker`,
		`Desk\.Speaker`:               "Desk.Speaker",
		`invalid\999`:                 `invalid\999`,
		`trailing\`:                   `trailing\`,
		"客厅":                          "客厅",
	} {
		if got := displayName(input); got != want {
			t.Errorf("displayName(%q) = %q, want %q", input, got, want)
		}
	}
	devices := livingRoom(t)
	for _, d := range devices {
		d.TXT["gpn"] = `Room\032Name`
	}
	if Targets(devices, time.Now())[0].Name != `Room\032Name` {
		t.Fatal("plain TXT group name was incorrectly DNS-decoded")
	}
}
