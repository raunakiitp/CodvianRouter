package config

import "testing"

func TestConfigTeamsValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Version: 1, Listen: "127.0.0.1:20128", DataDir: t.TempDir(), GatewayKeys: []GatewayKey{{ID: "k1"}, {ID: "k2"}}}
	}
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		mut  func(c *Config)
	}{
		{"valid one team", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "Alpha", MaxBudgetUSD: f(100), BudgetDuration: "7d", Members: []string{"k1"}}}
		}},
		{"valid budgetless", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "Alpha", Members: []string{"k1", "k2"}}}
		}},
		{"empty id", func(c *Config) {
			c.Teams = []TeamConfig{{Name: "Alpha"}}
		}},
		{"duplicate id", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A"}, {ID: "t1", Name: "B"}}
		}},
		{"empty name", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1"}}
		}},
		{"zero max", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", MaxBudgetUSD: f(0), BudgetDuration: "1d"}}
		}},
		{"duration without max", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", BudgetDuration: "1d"}}
		}},
		{"max without duration", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", MaxBudgetUSD: f(5)}}
		}},
		{"bad duration", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", MaxBudgetUSD: f(5), BudgetDuration: "year"}}
		}},
		{"bad status", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", Status: "deleted"}}
		}},
		{"unknown member", func(c *Config) {
			c.Teams = []TeamConfig{{ID: "t1", Name: "A", Members: []string{"nope"}}}
		}},
		{"key in two teams", func(c *Config) {
			c.Teams = []TeamConfig{
				{ID: "t1", Name: "A", Members: []string{"k1"}},
				{ID: "t2", Name: "B", Members: []string{"k1"}},
			}
		}},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(c)
		err := c.Validate()
		switch tc.name {
		case "valid one team", "valid budgetless":
			if err != nil {
				t.Errorf("%s: Validate = %v, want nil", tc.name, err)
			}
		default:
			if err == nil {
				t.Errorf("%s: Validate = nil, want an error", tc.name)
			}
		}
	}
}

func TestConfigTeamsCloneDeepCopies(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	c := &Config{Teams: []TeamConfig{{ID: "t1", Name: "Alpha", MaxBudgetUSD: f(100), BudgetDuration: "7d", Members: []string{"k1"}}}}
	cl := c.Clone()
	cl.Teams[0].Members[0] = "k2"
	*cl.Teams[0].MaxBudgetUSD = 5
	if c.Teams[0].Members[0] != "k1" || *c.Teams[0].MaxBudgetUSD != 100 {
		t.Errorf("clone shared state: %+v", c.Teams[0])
	}
}
