package dockercli

import "testing"

// Compose has emitted two different shapes for `ps --format json` over its v2
// lifetime, and which one a developer gets depends on their Docker Desktop
// version. Both must parse.
func TestDecodePsAcceptsArrayAndNDJSON(t *testing.T) {
	ndjson := `{"ID":"abc123def4567","Name":"shop-web-1","Service":"web","State":"running","Health":"healthy","Project":"shop","Publishers":[{"URL":"0.0.0.0","TargetPort":80,"PublishedPort":3000,"Protocol":"tcp"}]}
{"ID":"ffff0000","Name":"shop-db-1","Service":"db","State":"exited","Project":"shop","Publishers":[]}`

	arr := `[{"ID":"abc123def4567","Name":"shop-web-1","Service":"web","State":"running","Health":"healthy","Project":"shop","Publishers":[{"PublishedPort":3000,"Protocol":"tcp"}]},
	         {"ID":"ffff0000","Name":"shop-db-1","Service":"db","State":"exited","Project":"shop"}]`

	for name, input := range map[string]string{"ndjson": ndjson, "array": arr} {
		t.Run(name, func(t *testing.T) {
			entries, err := decodePs([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("got %d entries, want 2", len(entries))
			}
			if entries[0].Service != "web" || entries[0].State != "running" {
				t.Errorf("unexpected first entry: %+v", entries[0])
			}
			if entries[1].State != "exited" {
				t.Errorf("unexpected second entry: %+v", entries[1])
			}
		})
	}
}

func TestDecodePsEmptyOutput(t *testing.T) {
	// `compose ps` on a project that was never started prints nothing.
	entries, err := decodePs([]byte("   \n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
}

func TestParsePublishedPorts(t *testing.T) {
	cases := map[string][]int{
		"0.0.0.0:3000->3000/tcp, :::3000->3000/tcp": {3000},
		"0.0.0.0:8080->80/tcp, [::]:8080->80/tcp":   {8080, 8080},
		"127.0.0.1:5432->5432/tcp":                  {5432},
		"80/tcp":                                    nil,
		"":                                          nil,
		"0.0.0.0:1234->80/tcp, 0.0.0.0:5678->443/tcp": {1234, 5678},
	}
	for input, want := range cases {
		got := parsePublished(input)
		if len(got) != len(want) {
			t.Errorf("parsePublished(%q) = %v, want %v", input, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parsePublished(%q) = %v, want %v", input, got, want)
				break
			}
		}
	}
}

func TestLabelValue(t *testing.T) {
	labels := "com.docker.compose.service=web,com.docker.compose.project=shop,other=1"
	if got := labelValue(labels, "com.docker.compose.project"); got != "shop" {
		t.Errorf("labelValue = %q, want shop", got)
	}
	if got := labelValue(labels, "missing"); got != "" {
		t.Errorf("expected empty for a missing label, got %q", got)
	}
}

func TestComposeErrorKeepsLastLines(t *testing.T) {
	err := composeError(errAny{}, "line one\n\nline two\nline three\nline four\nline five\n")
	want := "line two; line three; line four; line five"
	if err.Error() != want {
		t.Errorf("composeError = %q, want %q", err.Error(), want)
	}
	if composeError(nil, "noise") != nil {
		t.Error("composeError(nil, …) should be nil")
	}
}

type errAny struct{}

func (errAny) Error() string { return "exit status 1" }

func TestShortID(t *testing.T) {
	if got := shortID("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("short IDs should pass through, got %q", got)
	}
}
