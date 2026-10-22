package daemonout

import "testing"

func TestStrip(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			// Copied from the lab daemon (gameap-daemon v4.1.5).
			name: "framed journalctl output",
			in:   "/# /usr/bin/env journalctl --unit=gameap-upd-x --output=cat\n\nStarting x...\nUpdated successfully\n\nExited with 0\n",
			want: "Starting x...\nUpdated successfully",
		},
		{
			name: "framed failure",
			in:   "/srv/gameap# /usr/bin/env gameapctl version\n\npermission denied\n\nExited with 2\n",
			want: "permission denied",
		},
		{
			name: "no framing",
			in:   "Version: v0.34.0\nBuild Date: x\n",
			want: "Version: v0.34.0\nBuild Date: x",
		},
		{
			name: "a '# ' later in the output is kept",
			in:   "first line\n# not a header\n",
			want: "first line\n# not a header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Strip(tt.in); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
