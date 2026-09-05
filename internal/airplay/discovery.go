package airplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
	"spoticonn/internal/model"
)

func Discover(ctx context.Context, iface string, found func(model.Device)) error {
	var opts []zeroconf.ClientOption
	if iface != "" {
		i, err := net.InterfaceByName(iface)
		if err != nil {
			return err
		}
		opts = append(opts, zeroconf.SelectIfaces([]net.Interface{*i}))
	}
	r, err := zeroconf.NewResolver(opts...)
	if err != nil {
		return err
	}
	entries := make(chan *zeroconf.ServiceEntry, 32)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-entries:
				if !ok {
					return
				}
				if d, ok := DeviceFromEntry(e); ok {
					found(d)
				}
			}
		}
	}()
	// Each physical device can advertise _airplay, including members of one
	// Apple TV/HomePod group. Targets combines those records for selection.
	return r.Browse(ctx, "_airplay._tcp", "local.", entries)
}

func DeviceFromEntry(e *zeroconf.ServiceEntry) (model.Device, bool) {
	if len(e.AddrIPv4) == 0 || e.Port <= 0 {
		return model.Device{}, false
	}
	txt := map[string]string{}
	for _, v := range e.Text {
		p := strings.SplitN(v, "=", 2)
		if len(p) == 2 {
			txt[p[0]] = p[1]
		}
	}
	id := strings.ToLower(strings.ReplaceAll(txt["deviceid"], ":", ""))
	if id == "" {
		sum := sha256.Sum256([]byte(e.Instance + "/" + e.HostName))
		id = hex.EncodeToString(sum[:12])
	}
	// Preserve DNS presentation names in stored transport records, as older
	// versions did. Decode exactly once when building the public target view.
	d := model.Device{ID: id, Name: e.Instance, Address: e.AddrIPv4[0].String(), Port: e.Port, Model: txt["model"], TXT: txt, LastSeen: time.Now(), Online: e.TTL > 0}
	return d, true
}
