package replay

import "testing"

func TestExpandCommand(t *testing.T) {
	cases := []struct {
		cmd, user, want string
	}{
		{"/eco give {SELF} 100000", "DEMO_00007", "eco give DEMO_00007 100000"},
		{"/ah sell 100", "DEMO_00000", "ah sell 100"},
		{"/ah", "X", "ah"},
		{"{SELF} bare", "P", "P bare"},               // no leading slash
		{"/msg {SELF} hi {SELF}", "A", "msg A hi A"}, // multiple tokens
		{"/", "P", ""},                               // slash only -> empty
		{"", "P", ""},
	}
	for _, c := range cases {
		got := expandCommand(c.cmd, c.user)
		if got != c.want {
			t.Errorf("expandCommand(%q,%q) = %q, want %q", c.cmd, c.user, got, c.want)
		}
	}
}
