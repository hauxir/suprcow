package compose

import "testing"

func TestSanitizeAllowsOrdinaryStack(t *testing.T) {
	// A realistic preview stack: built services, project-relative bind mounts
	// (including the whole project at "."), and named volumes.
	ok := `
services:
  db:
    image: postgres:18
    volumes:
      - pgdata:/var/lib/postgresql
  api:
    build:
      context: apps/backend
    volumes:
      - ./apps/backend:/app
      - cache:/cache
  web:
    build:
      context: apps/frontend
    volumes:
      - .:/app
volumes:
  pgdata:
  cache:
`
	if err := Sanitize([]byte(ok)); err != nil {
		t.Fatalf("expected safe stack to pass, got: %v", err)
	}
}

func TestSanitizeBlocksEscapes(t *testing.T) {
	cases := map[string]string{
		"privileged": `
services:
  x: { image: a, privileged: true }
`,
		"cap_add": `
services:
  x: { image: a, cap_add: ["SYS_ADMIN"] }
`,
		"devices": `
services:
  x:
    image: a
    devices: ["/dev/kmsg:/dev/kmsg"]
`,
		"security_opt": `
services:
  x:
    image: a
    security_opt: ["apparmor:unconfined"]
`,
		"pid host": `
services:
  x: { image: a, pid: host }
`,
		"network_mode host": `
services:
  x: { image: a, network_mode: host }
`,
		"docker socket bind": `
services:
  x:
    image: a
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
`,
		"root bind": `
services:
  x:
    image: a
    volumes:
      - /:/host
`,
		"home bind": `
services:
  x:
    image: a
    volumes:
      - ~/.ssh:/keys
`,
		"parent escape": `
services:
  x:
    image: a
    volumes:
      - ../../etc:/etc2
`,
		"long-syntax bind absolute": `
services:
  x:
    image: a
    volumes:
      - { type: bind, source: /etc, target: /etc2 }
`,
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Sanitize([]byte(yml)); err == nil {
				t.Fatalf("expected %q to be rejected, but it passed", name)
			}
		})
	}
}

func TestSanitizeAllowsSafeNamespaceAndDropCaps(t *testing.T) {
	ok := `
services:
  x:
    image: a
    cap_drop: ["ALL"]
    privileged: false
    ipc: shareable
    volumes:
      - data:/data
volumes:
  data:
`
	if err := Sanitize([]byte(ok)); err != nil {
		t.Fatalf("expected safe config to pass, got: %v", err)
	}
}
