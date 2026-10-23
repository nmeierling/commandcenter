package composefile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePortSyntaxes(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "compose.yaml", `
name: shop
services:
  web:
    image: nginx
    ports:
      - "3000:80"
      - "127.0.0.1:3010:80"
      - "8080"
      - "6060:6060/udp"
  api:
    ports:
      - target: 8000
        published: "4000"
      - target: 9000
  range:
    ports:
      - "7000-7002:80"
  ipv6:
    ports:
      - "[::1]:5000:80"
`)

	name, services, err := Parse(path)
	ports := AllPorts(services)
	if err != nil {
		t.Fatal(err)
	}
	if name != "shop" {
		t.Errorf("project name = %q, want shop", name)
	}

	got := map[int]string{}
	for _, p := range ports {
		got[p.Host] = p.Service
	}

	// Published host ports, including the UDP one (the host port is still
	// occupied) and every range member.
	for _, want := range []int{3000, 3010, 6060, 4000, 7000, 7001, 7002, 5000} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected host port %d in %v", want, got)
		}
	}
	// "8080" alone is a container port, and a long entry without `published`
	// publishes nothing: neither should appear as a host port.
	if _, ok := got[8080]; ok {
		t.Error("container-only port 8080 should not be reported as a host port")
	}
	if _, ok := got[9000]; ok {
		t.Error("long-form port without `published` should not be reported")
	}
	if got[3000] != "web" || got[4000] != "api" {
		t.Errorf("wrong service attribution: %v", got)
	}
}

func TestParseSortsPortsForStableUI(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "compose.yaml", `
services:
  c: {ports: ["9000:80"]}
  a: {ports: ["3000:80"]}
  b: {ports: ["5000:80"]}
`)
	// Service maps iterate in random order; ports must not reshuffle between
	// refreshes or the dashboard flickers.
	for i := 0; i < 20; i++ {
		_, services, err := Parse(path)
		ports := AllPorts(services)
		if err != nil {
			t.Fatal(err)
		}
		if len(ports) != 3 || ports[0].Host != 3000 || ports[1].Host != 5000 || ports[2].Host != 9000 {
			t.Fatalf("unsorted ports on iteration %d: %v", i, ports)
		}
	}
}

func TestParseInterpolatesFromDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "DB_PORT=15432\nexport WEB_PORT=\"3100\"\n# comment\n")
	path := writeFile(t, dir, "compose.yaml", `
services:
  db:
    ports: ["${DB_PORT}:5432"]
  web:
    ports: ["${WEB_PORT:-3000}:80"]
  fallback:
    ports: ["${NOT_SET:-4444}:80"]
`)

	_, services, err := Parse(path)
	ports := AllPorts(services)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, p := range ports {
		got[p.Host] = true
	}
	for _, want := range []int{15432, 3100, 4444} {
		if !got[want] {
			t.Errorf("expected port %d, got %v", want, got)
		}
	}
}

func TestShellEnvironmentBeatsDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "DB_PORT=15432\n")
	path := writeFile(t, dir, "compose.yaml", `
services:
  db:
    ports: ["${DB_PORT}:5432"]
`)
	t.Setenv("DB_PORT", "25432")

	_, services, err := Parse(path)
	ports := AllPorts(services)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 || ports[0].Host != 25432 {
		t.Errorf("shell environment should win over .env, got %v", ports)
	}
}

func TestParseAbsurdRangeIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "compose.yaml", `
services:
  wide:
    ports: ["1000-60000:80"]
`)
	_, services, err := Parse(path)
	ports := AllPorts(services)
	if err != nil {
		t.Fatal(err)
	}
	// A 59000-wide range would swamp the UI; we drop it rather than expand it.
	if len(ports) != 0 {
		t.Errorf("expected an oversized range to be ignored, got %d ports", len(ports))
	}
}

func TestFindPrefersCanonicalName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docker-compose.yml", "services: {}")
	if got := Find(dir); filepath.Base(got) != "docker-compose.yml" {
		t.Errorf("Find = %q", got)
	}
	writeFile(t, dir, "compose.yaml", "services: {}")
	if got := Find(dir); filepath.Base(got) != "compose.yaml" {
		t.Errorf("compose.yaml should win, got %q", got)
	}
	if got := Find(t.TempDir()); got != "" {
		t.Errorf("expected empty for a directory with no compose file, got %q", got)
	}
}

func TestParseListsEveryServiceIncludingPortless(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx
    ports: ["3000:80"]
  worker:
    image: alpine
    command: ["sleep", "60"]
  db:
    image: postgres
    ports: ["5432:5432"]
`)
	_, services, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	// A service with no published ports is still part of the project, and the
	// dashboard lists it whatever its state.
	if len(services) != 3 {
		t.Fatalf("got %d services, want 3: %+v", len(services), services)
	}
	// Sorted, so rows do not reshuffle between refreshes.
	if services[0].Name != "db" || services[1].Name != "web" || services[2].Name != "worker" {
		t.Errorf("services should be sorted by name, got %+v", services)
	}
	for _, svc := range services {
		if svc.Name == "worker" && len(svc.Ports) != 0 {
			t.Errorf("worker should have no ports, got %+v", svc.Ports)
		}
		if svc.Name == "web" && (len(svc.Ports) != 1 || svc.Ports[0].Host != 3000 || svc.Ports[0].Container != 80) {
			t.Errorf("web ports = %+v, want 3000->80", svc.Ports)
		}
	}
}

// Compose loads an override sibling automatically, but only when no -f is
// passed. The agent always passes -f, so it has to find the sibling itself.
func TestFindOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docker-compose.yml", "services: {}")
	if got := FindOverride(dir); got != "" {
		t.Errorf("no override present, got %q", got)
	}

	writeFile(t, dir, "docker-compose.override.yml", "services: {}")
	if got := FindOverride(dir); filepath.Base(got) != "docker-compose.override.yml" {
		t.Errorf("FindOverride = %q", got)
	}

	// The modern name wins, matching Compose's own precedence.
	writeFile(t, dir, "compose.override.yaml", "services: {}")
	if got := FindOverride(dir); filepath.Base(got) != "compose.override.yaml" {
		t.Errorf("compose.override.yaml should win, got %q", got)
	}
}
