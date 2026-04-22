package manifest

import (
	"strings"
	"testing"
)

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name string
		src  string
		env  map[string]string
		want string
		err  string
	}{
		{
			name: "simple substitution",
			src:  `image = "app:${VERSION}"`,
			env:  map[string]string{"VERSION": "1.2.3"},
			want: `image = "app:1.2.3"`,
		},
		{
			name: "default when unset",
			src:  `tag = "${TAG:-latest}"`,
			env:  map[string]string{},
			want: `tag = "latest"`,
		},
		{
			name: "empty default uses empty",
			src:  `debug = "${DEBUG:-}"`,
			env:  map[string]string{},
			want: `debug = ""`,
		},
		{
			name: "missing var without default is error",
			src:  `x = "${MISSING}"`,
			env:  map[string]string{},
			err:  "undefined variable(s): MISSING",
		},
		{
			name: "env map wins over os env",
			src:  `x = "${HOME}"`,
			env:  map[string]string{"HOME": "override"},
			want: `x = "override"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Interpolate(tc.src, tc.env)

			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want err %q, got %v", tc.err, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}

			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
