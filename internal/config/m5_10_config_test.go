package config

import "testing"

// M5.10: per-key access schedule / throttle delay / max sessions
// — config validation + Clone.
func TestConfigKeyAccessScheduleValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{{
			ID: "k1", Name: "k1", KeyHash: "h1",
			AccessSchedule: &AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{1, 2, 3, 4, 5}, TZ: "UTC"},
		}}
		return cfg
	}
	cases := []struct {
		name string
		set  func(*Config)
		ok   bool
	}{
		{"valid", func(c *Config) {}, true},
		{"disabled schedule needs no times", func(c *Config) {
			c.GatewayKeys[0].AccessSchedule.Enabled = false
			c.GatewayKeys[0].AccessSchedule.From = ""
			c.GatewayKeys[0].AccessSchedule.Until = ""
		}, true},
		{"no schedule at all", func(c *Config) { c.GatewayKeys[0].AccessSchedule = nil }, true},
		{"enabled without from", func(c *Config) { c.GatewayKeys[0].AccessSchedule.From = "" }, false},
		{"enabled without until", func(c *Config) { c.GatewayKeys[0].AccessSchedule.Until = "" }, false},
		{"from 9am (no leading zero)", func(c *Config) { c.GatewayKeys[0].AccessSchedule.From = "9:00" }, false},
		{"from 25:00", func(c *Config) { c.GatewayKeys[0].AccessSchedule.From = "25:00" }, false},
		{"until 17:60", func(c *Config) { c.GatewayKeys[0].AccessSchedule.Until = "17:60" }, false},
		{"day 7", func(c *Config) { c.GatewayKeys[0].AccessSchedule.Days = []int{7} }, false},
		{"day -1", func(c *Config) { c.GatewayKeys[0].AccessSchedule.Days = []int{-1} }, false},
		{"bad tz", func(c *Config) { c.GatewayKeys[0].AccessSchedule.TZ = "No/Such_Zone" }, false},
		{"overnight window ok", func(c *Config) {
			c.GatewayKeys[0].AccessSchedule.From = "22:00"
			c.GatewayKeys[0].AccessSchedule.Until = "06:00"
		}, true},
		{"negative throttle", func(c *Config) { c.GatewayKeys[0].ThrottleDelayMs = -1 }, false},
		{"negative max sessions", func(c *Config) { c.GatewayKeys[0].MaxSessions = -1 }, false},
		{"zero throttle and sessions ok", func(c *Config) { c.GatewayKeys[0].ThrottleDelayMs = 0; c.GatewayKeys[0].MaxSessions = 0 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.set(c)
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestConfigKeyAccessScheduleCloneDeepCopies(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.GatewayKeys = []GatewayKey{{
		ID: "k1", Name: "k1", KeyHash: "h1",
		AccessSchedule: &AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{1, 2}, TZ: "UTC"},
	}}
	clone := c.Clone()
	clone.GatewayKeys[0].AccessSchedule.Days[0] = 6
	clone.GatewayKeys[0].AccessSchedule.From = "10:00"
	if c.GatewayKeys[0].AccessSchedule.Days[0] != 1 || c.GatewayKeys[0].AccessSchedule.From != "09:00" {
		t.Errorf("clone shares the access schedule with the original: %+v", c.GatewayKeys[0].AccessSchedule)
	}
}
