package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestNginxTemplateReplacesCallerSuppliedIdentityHeaders(t *testing.T) {
	content, err := os.ReadFile("aperod.com.nginx")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, unsafe := range []string{"$proxy_add_x_forwarded_for", "$http_cf_connecting_ip"} {
		if strings.Contains(text, unsafe) {
			t.Fatalf("untrusted caller identity remains in nginx template: %s", unsafe)
		}
	}
	if got := strings.Count(text, "X-Forwarded-For   $remote_addr;"); got != 3 {
		t.Fatalf("only %d proxy locations replace X-Forwarded-For, want 3", got)
	}
	if got := strings.Count(text, "CF-Connecting-IP  $remote_addr;"); got != 2 {
		t.Fatalf("only %d API proxy locations replace CF-Connecting-IP, want 2", got)
	}
}
