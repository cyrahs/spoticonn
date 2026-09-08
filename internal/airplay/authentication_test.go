package airplay

import (
	"testing"
	"time"

	"spoticonn/internal/model"
)

func TestAuthenticationUsesCapabilitiesAndPolicyNotModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		txt  map[string]string
		want string
	}{
		{"transient HomePod", map[string]string{"features": "0x0,0x10000", "flags": "0x4"}, "none"},
		{"HK pairing capability", map[string]string{"ft": "0,4040", "sf": "4"}, "none"},
		{"PIN flag", map[string]string{"flags": "0x8"}, "pin"},
		{"legacy PIN flag", map[string]string{"sf": "200"}, "pin"},
		{"password flag", map[string]string{"sf": "0x80"}, "password"},
		{"password TXT", map[string]string{"pw": "TRUE"}, "password"},
		{"password over PIN", map[string]string{"flags": "0x288"}, "password"},
		{"home access", map[string]string{"acl": "1", "pw": "true", "flags": "0x8"}, "access_control"},
		{"current user", map[string]string{"act": "2"}, "access_control"},
		{"missing data", nil, "unknown"},
		{"missing status", map[string]string{"features": "0,10000"}, "unknown"},
		{"missing capabilities", map[string]string{"flags": "0"}, "unknown"},
		{"unusable capabilities", map[string]string{"flags": "0", "features": "0,0"}, "unknown"},
		{"malformed status", map[string]string{"features": "0,10000", "flags": "n/a"}, "unknown"},
		{"malformed half", map[string]string{"features": "0,100000000", "flags": "4"}, "unknown"},
		{"trailing garbage", map[string]string{"features": "0,10000garbage", "flags": "4"}, "unknown"},
		{"unfamiliar policy", map[string]string{"features": "0,10000", "flags": "4", "act": "7"}, "unknown"},
		{"unfamiliar password flag", map[string]string{"features": "0,10000", "flags": "4", "pw": "maybe"}, "unknown"},
		{"conflicting aliases", map[string]string{"features": "0,10000", "flags": "4", "sf": "8"}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, modelName := range []string{"AudioAccessory6,1", "AppleTV14,1", "OtherSpeaker"} {
				d := model.Device{Model: modelName, TXT: tc.txt, Online: true}
				got := Authentication(d, model.PairingSecret{})
				if got.Requirement != tc.want || got.PasswordSaved {
					t.Fatalf("%s: got %+v, want %s", modelName, got, tc.want)
				}
			}
		})
	}
}

func TestAuthenticationIsPerMemberAndUpdatesWithDiscovery(t *testing.T) {
	devices := livingRoom(t)
	podID, tvID := "6a9cdd7721c7", "0603165e17b1"
	devices[podID].TXT["features"] = "0,10000"
	devices[podID].TXT["flags"] = "4"
	devices[tvID].TXT["flags"] = "8"
	view := Targets(devices, time.Now())[0].View(nil)
	if view.Authentication.Requirement != "none" || view.Members[0].Authentication.Requirement != "pin" || view.Members[1].Authentication.Requirement != "none" {
		t.Fatal("TV pairing requirement was applied to HomePod")
	}
	devices[podID].TXT["pw"] = "true"
	secrets := map[string]model.PairingSecret{podID: {Password: "saved-password"}, tvID: {Credentials: "saved-keys"}}
	view = Targets(devices, time.Now())[0].View(secrets)
	if view.Paired || !view.Authentication.PasswordSaved || view.Authentication.Requirement != "password" || !view.Members[0].Paired {
		t.Fatal("password was treated as a pairing, or policy update ignored")
	}
	delete(devices[podID].TXT, "pw")
	d := devices[podID]
	d.LastSeen = time.Now().Add(-3 * time.Minute)
	devices[podID] = d
	view = Targets(devices, time.Now())[0].View(secrets)
	if view.Authentication.Requirement != "unknown" || !view.Authentication.PasswordSaved {
		t.Fatal("stale discovery claimed open access or lost saved password")
	}
}
