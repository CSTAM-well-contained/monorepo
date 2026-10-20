package haproxy

import "testing"

func TestCheckErrors(t *testing.T) {
	sample := `[NOTICE]   (835) : haproxy version is 3.2.26-3e0f11a
[NOTICE]   (835) : path to executable is /usr/local/sbin/haproxy
[ALERT]    (835) : config : parsing [/tmp/b.cfg:2] : unknown keyword 'nosuchkeyword' in 'global' section
[ALERT]    (835) : config : Error(s) found in configuration file : /tmp/b.cfg
`
	cases := []struct{ name, in, want string }{
		{"alerts only", sample, "[ALERT]    (835) : config : parsing [/tmp/b.cfg:2] : unknown keyword 'nosuchkeyword' in 'global' section" +
			" | [ALERT]    (835) : config : Error(s) found in configuration file : /tmp/b.cfg"},
		{"no alert: everything", "  something else failed\n", "something else failed"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckErrors(tc.in); got != tc.want {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}
