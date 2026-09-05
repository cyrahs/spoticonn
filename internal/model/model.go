package model

import "time"

type Settings struct {
	Name     string `json:"name"`
	TargetID string `json:"target_id"`
	Volume   int    `json:"volume"`
}

type Account struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Username string    `json:"username"`
	DeviceID string    `json:"device_id"`
	AddedAt  time.Time `json:"added_at"`
	Bound    bool      `json:"bound"`
}

type Device struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Address  string            `json:"address"`
	Port     int               `json:"port"`
	Model    string            `json:"model"`
	TXT      map[string]string `json:"txt,omitempty"`
	LastSeen time.Time         `json:"last_seen"`
	Online   bool              `json:"online"`
	Paired   bool              `json:"paired"`
}

type PairingSecret struct {
	DACP        string `json:"dacp"`
	Credentials string `json:"credentials"`
}

type Track struct {
	URI        string   `json:"uri"`
	Name       string   `json:"name"`
	Artists    []string `json:"artist_names"`
	Album      string   `json:"album_name"`
	Cover      string   `json:"album_cover_url"`
	Position   int64    `json:"position"`
	Duration   int64    `json:"duration"`
	SampleRate int      `json:"sample_rate"`
}

type PlayerStatus struct {
	Username    string `json:"username"`
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	Stopped     bool   `json:"stopped"`
	Paused      bool   `json:"paused"`
	Buffering   bool   `json:"buffering"`
	Volume      int    `json:"volume"`
	VolumeSteps int    `json:"volume_steps"`
	Track       *Track `json:"track"`
}

type AccountView struct {
	Account
	Status        string             `json:"status"`
	Error         string             `json:"error,omitempty"`
	Authorization *AuthorizationView `json:"authorization,omitempty"`
}

type AuthorizationView struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type PairingView struct {
	ID       string `json:"id"`
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type Playback struct {
	AccountID    string    `json:"account_id"`
	Status       string    `json:"status"`
	OutputStatus string    `json:"output_status"`
	Error        string    `json:"error,omitempty"`
	Recovering   bool      `json:"recovering"`
	Track        *Track    `json:"track"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Diagnostic struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

type Snapshot struct {
	Settings         Settings      `json:"settings"`
	Accounts         []AccountView `json:"accounts"`
	Devices          []Device      `json:"devices"`
	Pairing          *PairingView  `json:"pairing"`
	Playback         Playback      `json:"playback"`
	Diagnostics      []Diagnostic  `json:"diagnostics"`
	SpotifyAvailable bool          `json:"spotify_available"`
	AirPlayAvailable bool          `json:"airplay_available"`
}
