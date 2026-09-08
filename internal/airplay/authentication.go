package airplay

import (
	"errors"
	"strconv"
	"strings"

	"spoticonn/internal/model"
)

var (
	ErrAuthRequired = errors.New("设备要求认证，请检查该成员的密码、配对码或家庭访问设置")
	ErrAuthFailed   = errors.New("设备拒绝认证，请检查该成员已保存的密码、配对凭据及家庭访问设置")
)

// Authentication follows cliairplay v0.5.3's advertised pairing capabilities
// (features 38/46/48, flags 0x8/0x200), with password and access restrictions
// interpreted as in pyatv's AirPlay service discovery. No model-name shortcut:
// the same HomePod can advertise different requirements under another policy.
func Authentication(d model.Device, secret model.PairingSecret) model.Authentication {
	a := model.Authentication{Requirement: "unknown", PasswordSaved: secret.Password != ""}
	flags, flagsOK := txtMask(d.TXT, "flags", "sf")
	features, featuresOK := txtMask(d.TXT, "features", "ft")
	pw := strings.ToLower(strings.TrimSpace(d.TXT["pw"]))
	acl, act := d.TXT["acl"], d.TXT["act"]
	switch {
	case acl == "1" || act == "2":
		a.Requirement = "access_control"
	case pw == "true" || (flagsOK && flags&0x80 != 0):
		a.Requirement = "password"
	case flagsOK && flags&(0x8|0x200) != 0:
		a.Requirement = "pin"
	case !d.Online || !flagsOK || !featuresOK || (pw != "" && pw != "false") ||
		(acl != "" && acl != "0") || (act != "" && act != "0"):
		// Stale, incomplete or unfamiliar policy data cannot establish open access.
	case features&((1<<38)|(1<<48)) != 0 && features&((1<<46)|(1<<48)) != 0:
		a.Requirement = "none"
	}
	return a
}

// TXT masks are hexadecimal even without a 0x prefix. Features may have two
// 32-bit halves in LOW,HIGH order. Conflicting aliases remain unknown.
func txtMask(txt map[string]string, keys ...string) (uint64, bool) {
	var mask uint64
	found := false
	for _, key := range keys {
		raw, exists := txt[key]
		if !exists {
			continue
		}
		parts := strings.Split(raw, ",")
		if len(parts) > 2 || (len(parts) == 2 && key != "features" && key != "ft") {
			return 0, false
		}
		var value uint64
		for i, part := range parts {
			part = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(part)), "0x")
			bits := 64
			if len(parts) == 2 {
				bits = 32
			}
			n, err := strconv.ParseUint(part, 16, bits)
			if err != nil {
				return 0, false
			}
			value |= n << (32 * i)
		}
		if found && mask != value {
			return 0, false
		}
		mask, found = value, true
	}
	return mask, found
}
