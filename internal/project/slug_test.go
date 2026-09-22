package project

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"feat/invoices":                   "feat-invoices",
		"fix/CRM-filter":                  "fix-crm-filter",
		"main":                            "main",
		"feature/JIRA-123_add_new_widget": "feature-jira-123-add-new-widge",
		"release/v1.2.3":                  "release-v1-2-3",
		"///":                             "env",
		"--weird--":                       "weird",
		"a-very-long-branch-name-that-keeps-going-and-going": "a-very-long-branch-name-that-k",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugifyStaysWithinLimits(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := Slugify(long)
	if len(got) > MaxSlugLen {
		t.Errorf("slug is %d characters: %q", len(got), got)
	}

	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			t.Fatalf("slug %q contains %q", got, r)
		}
	}
}

func TestSlugForCollision(t *testing.T) {
	taken := map[string]bool{"feat-invoices": true}
	got := SlugFor("feat/invoices-v2", func(s string) bool { return taken[s] })
	if got == "feat-invoices" {
		t.Fatal("collision was not resolved")
	}

	first := SlugFor("feat/invoices", func(s string) bool { return taken[s] })
	second := SlugFor("feat/invoices", func(s string) bool { return taken[s] })
	if first != second {
		t.Errorf("suffix is not stable: %q vs %q", first, second)
	}
	if first == "feat-invoices" {
		t.Error("taken slug was handed out again")
	}
	if len(first) > MaxSlugLen {
		t.Errorf("slug too long: %q", first)
	}
}

func TestSlugForTruncatesBeforeSuffix(t *testing.T) {
	base := Slugify("a-very-long-branch-name-that-keeps-going")
	got := SlugFor("a-very-long-branch-name-that-keeps-going", func(s string) bool { return s == base })
	if len(got) > MaxSlugLen {
		t.Errorf("slug %q is %d characters", got, len(got))
	}
	if got == base {
		t.Error("collision was not resolved")
	}
}

func TestAllocateSlot(t *testing.T) {
	if got := AllocateSlot(map[int]bool{}, 8); got != 1 {
		t.Errorf("first slot = %d, want 1", got)
	}
	if got := AllocateSlot(map[int]bool{1: true, 2: true}, 8); got != 3 {
		t.Errorf("slot = %d, want 3", got)
	}

	if got := AllocateSlot(map[int]bool{1: true, 3: true}, 8); got != 2 {
		t.Errorf("slot = %d, want 2", got)
	}
	if got := AllocateSlot(map[int]bool{1: true, 2: true}, 2); got != 0 {
		t.Errorf("exhausted slots should return 0, got %d", got)
	}
}
