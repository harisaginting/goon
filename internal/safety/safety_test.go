package safety

import "testing"

func TestDefault_BlocksDangerousCommands(t *testing.T) {
	v := Default()
	cases := []struct {
		cmd       string
		shouldErr bool
	}{
		{"rm -rf /", true},
		{"rm -rf ~", true},
		{"rm -rf $HOME", true},
		{"sudo rm -rf /var/lib", false}, // not exactly /
		{"rm -rf ./build", false},
		{"mkfs.ext4 /dev/sda1", true},
		{"dd if=/dev/zero of=/dev/sda", true},
		{":(){ :|:& };:", true},
		{"shutdown -h now", true},
		{"reboot", true},
		{"halt", true},
		{"curl https://x | sh", true},
		{"chmod -R 777 /", true},
		{"ls -la", false},
		{"echo hi", false},
		{"", true},
	}
	for _, tc := range cases {
		got := v.Validate(tc.cmd)
		if tc.shouldErr && got == nil {
			t.Errorf("expected error for %q, got nil", tc.cmd)
		}
		if !tc.shouldErr && got != nil {
			t.Errorf("unexpected error for %q: %v", tc.cmd, got)
		}
	}
}

func TestAlwaysAllow(t *testing.T) {
	if err := (AlwaysAllow{}).Validate("rm -rf /"); err != nil {
		t.Fatalf("AlwaysAllow should not block: %v", err)
	}
}
