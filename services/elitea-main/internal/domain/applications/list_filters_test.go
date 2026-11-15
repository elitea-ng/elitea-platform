package applications

import "testing"

func TestValidateListStatusAcceptsSeveralStates(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", "", false},
		{"draft", "draft", false},
		{"Published", "published", false},
		{"draft,published", "", false}, // both states: no filter
		{"draft, draft", "draft", false},
		{"retired", "", true},
		{"draft,retired", "", true},
	}
	for _, c := range cases {
		req := ListRequest{Status: c.in}
		err := req.ValidateList()
		if (err != nil) != c.wantErr {
			t.Fatalf("status %q: err = %v, wantErr %v", c.in, err, c.wantErr)
		}
		if err == nil && req.Status != c.want {
			t.Errorf("status %q normalised to %q, want %q", c.in, req.Status, c.want)
		}
	}
}
