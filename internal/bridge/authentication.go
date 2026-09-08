package bridge

import (
	"errors"
	"strings"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

// Called with op held so PIN and password operations resolve the same physical
// member and never copy secrets between the members of a selectable group.
func (m *Manager) authenticationMember(deviceID, memberID string) (airplay.Target, model.Device, error) {
	m.mu.Lock()
	target, exists := airplay.ResolveTarget(m.devices, deviceID, time.Now())
	m.mu.Unlock()
	if !exists {
		return target, model.Device{}, errors.New("设备不存在")
	}
	d := target.Device
	if pod, _, staged := target.HomeTheater(); staged {
		d = pod
		if memberID != "" {
			found := false
			for _, member := range target.Members {
				if member.ID == memberID {
					d, found = member, true
				}
			}
			if !found {
				return target, d, errors.New("认证成员不属于所选组合")
			}
		}
	} else {
		if memberID != "" && memberID != d.ID {
			return target, d, errors.New("此目标不支持独立成员认证")
		}
		if err := target.Ready(); err != nil {
			return target, d, err
		}
	}
	if !d.Online {
		return target, d, errors.New("认证成员尚未上线")
	}
	return target, d, nil
}

// SaveDevicePassword stores an AirPlay password, not a four-digit screen PIN.
// The engine verifies it on the next connection; saving never asserts success
// or interrupts existing playback, and preserves any paired identity.
func (m *Manager) SaveDevicePassword(deviceID, memberID, password string) error {
	m.op.Lock()
	defer m.op.Unlock()
	if password == "" || len(password) > 256 || strings.ContainsAny(password, "\x00\r\n") {
		return errors.New("请输入 1–256 字节的设备密码，不能包含换行或空字符")
	}
	target, d, err := m.authenticationMember(deviceID, memberID)
	if err != nil {
		return err
	}
	if airplay.Authentication(d, model.PairingSecret{}).Requirement == "access_control" {
		return errors.New("设备限制了家庭访问权限，请先检查 AirPlay 访问设置；密码不能代替家庭访问授权")
	}
	err = m.store.Update(func(s *store.State) error {
		secret := s.Pairings[d.ID]
		secret.Password = password
		s.Pairings[d.ID] = secret
		for _, member := range target.Members {
			s.Devices[member.ID] = member
		}
		return nil
	})
	if err == nil {
		m.changed()
	}
	return err
}
