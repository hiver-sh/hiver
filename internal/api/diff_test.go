package api

import (
	"testing"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
)

func localFS(mount string, acls *[]gen.ACLRule) gen.FileSystem {
	var fs gen.FileSystem
	_ = fs.FromLocalFileSystem(gen.LocalFileSystem{
		Mount:   mount,
		Backend: "local",
		Acls:    acls,
	})
	return fs
}

func gdriveFS(mount string, acls *[]gen.ACLRule) gen.FileSystem {
	var fs gen.FileSystem
	_ = fs.FromGDriveFileSystem(gen.GDriveFileSystem{
		Mount:   mount,
		Backend: "gdrive",
		Acls:    acls,
	})
	return fs
}

func gcsFS(mount string, acls *[]gen.ACLRule) gen.FileSystem {
	var fs gen.FileSystem
	_ = fs.FromGCSFileSystem(gen.GCSFileSystem{
		Mount:   mount,
		Backend: "gcs",
		Acls:    acls,
	})
	return fs
}

func aclsRW(mount string) *[]gen.ACLRule {
	return &[]gen.ACLRule{{Path: mount + "/**", Access: gen.ACLRuleAccessRw}}
}

func TestNormalizeConfig_DefaultACL(t *testing.T) {
	cases := []struct {
		name  string
		input gen.FileSystem
		mount string
	}{
		{"local nil acls", localFS("/workspace", nil), "/workspace"},
		{"local empty acls", localFS("/data", &[]gen.ACLRule{}), "/data"},
		{"gdrive nil acls", gdriveFS("/drive", nil), "/drive"},
		{"gcs nil acls", gcsFS("/bucket", nil), "/bucket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gen.SandboxConfig{Fs: []gen.FileSystem{tc.input}}
			got := NormalizeConfig(cfg)
			acls := FSBase(got.Fs[0]).Acls
			if acls == nil || len(*acls) != 1 {
				t.Fatalf("want 1 ACL rule, got %v", acls)
			}
			want := tc.mount + "/**"
			if (*acls)[0].Path != want || (*acls)[0].Access != gen.ACLRuleAccessRw {
				t.Errorf("got %+v, want {%s rw}", (*acls)[0], want)
			}
		})
	}
}

func TestNormalizeConfig_ExplicitACLsUnchanged(t *testing.T) {
	explicit := &[]gen.ACLRule{
		{Path: "/workspace/secret/**", Access: gen.ACLRuleAccessDeny},
		{Path: "/workspace/**", Access: gen.ACLRuleAccessRo},
	}
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", explicit)}}
	got := NormalizeConfig(cfg)
	acls := FSBase(got.Fs[0]).Acls
	if acls == nil || len(*acls) != 2 {
		t.Fatalf("want 2 ACL rules, got %v", acls)
	}
	if (*acls)[0].Path != "/workspace/secret/**" || (*acls)[0].Access != gen.ACLRuleAccessDeny {
		t.Errorf("first rule changed: %+v", (*acls)[0])
	}
}

func TestNormalizeConfig_MultipleMounts(t *testing.T) {
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", nil),
		localFS("/data", aclsRW("/data")),
		gcsFS("/bucket", nil),
	}}
	got := NormalizeConfig(cfg)

	for _, tc := range []struct {
		idx   int
		mount string
	}{
		{0, "/workspace"},
		{2, "/bucket"},
	} {
		acls := FSBase(got.Fs[tc.idx]).Acls
		if acls == nil || len(*acls) != 1 || (*acls)[0].Path != tc.mount+"/**" {
			t.Errorf("fs[%d]: want default acl for %s, got %v", tc.idx, tc.mount, acls)
		}
	}

	// fs[1] had explicit acls — must be untouched
	acls1 := FSBase(got.Fs[1]).Acls
	if acls1 == nil || len(*acls1) != 1 || (*acls1)[0].Path != "/data/**" {
		t.Errorf("fs[1]: explicit acls changed: %v", acls1)
	}
}

func TestDiffConfig_NoChange(t *testing.T) {
	fs := localFS("/workspace", aclsRW("/workspace"))
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{fs}}
	ch := diffConfig(cfg, cfg)
	if ch.Fs != nil || ch.Egress != nil {
		t.Errorf("expected empty diff, got %+v", ch)
	}
}

func TestDiffConfig_FSAdded(t *testing.T) {
	current := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", aclsRW("/workspace"))}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", aclsRW("/workspace")),
		localFS("/data", aclsRW("/data")),
	}}
	ch := diffConfig(current, desired)
	if ch.Fs == nil || ch.Fs.Added == nil || len(*ch.Fs.Added) != 1 {
		t.Fatalf("want 1 added fs, got %+v", ch.Fs)
	}
	if FSBase((*ch.Fs.Added)[0]).Mount != "/data" {
		t.Errorf("wrong added mount: %+v", (*ch.Fs.Added)[0])
	}
	if ch.Fs.Removed != nil {
		t.Errorf("unexpected removed: %v", ch.Fs.Removed)
	}
}

func TestDiffConfig_FSRemoved(t *testing.T) {
	current := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", aclsRW("/workspace")),
		localFS("/data", aclsRW("/data")),
	}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", aclsRW("/workspace"))}}
	ch := diffConfig(current, desired)
	if ch.Fs == nil || ch.Fs.Removed == nil || len(*ch.Fs.Removed) != 1 {
		t.Fatalf("want 1 removed fs, got %+v", ch.Fs)
	}
	if FSBase((*ch.Fs.Removed)[0]).Mount != "/data" {
		t.Errorf("wrong removed mount: %+v", (*ch.Fs.Removed)[0])
	}
}

func TestDiffConfig_ACLChange(t *testing.T) {
	current := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", aclsRW("/workspace"))}}
	roACLs := &[]gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRo}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", roACLs)}}
	ch := diffConfig(current, desired)
	if ch.Fs == nil || ch.Fs.Added == nil || ch.Fs.Removed == nil {
		t.Fatalf("want add+remove for acl change, got %+v", ch.Fs)
	}
	if len(*ch.Fs.Added) != 1 || len(*ch.Fs.Removed) != 1 {
		t.Errorf("want 1 added and 1 removed, got added=%d removed=%d", len(*ch.Fs.Added), len(*ch.Fs.Removed))
	}
}
