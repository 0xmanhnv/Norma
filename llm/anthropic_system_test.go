package llm

import "testing"

func TestBuildAnthropicSystemCacheBoundary(t *testing.T) {
	sys := []string{"a", "b", "c"}

	cases := []struct {
		name       string
		boundary   int
		wantBlocks int
		wantCached []bool // per-block: has cache_control
	}{
		{"no boundary (0) → whole thing cached", 0, 1, []bool{true}},
		{"negative → whole thing cached", -1, 1, []bool{true}},
		{"mid split → cached prefix + dynamic tail", 2, 2, []bool{true, false}},
		{"boundary == len → whole thing cached", 3, 1, []bool{true}},
		{"boundary > len → whole thing cached", 5, 1, []bool{true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks := buildAnthropicSystem(CompletionRequest{System: sys, DynamicBoundary: c.boundary})
			if len(blocks) != c.wantBlocks {
				t.Fatalf("blocks=%d want %d", len(blocks), c.wantBlocks)
			}
			for i, wantCached := range c.wantCached {
				gotCached := blocks[i].CacheControl != nil
				if gotCached != wantCached {
					t.Fatalf("block %d cached=%v want %v", i, gotCached, wantCached)
				}
			}
		})
	}
}

// DisableCache keeps the same split and drops only the marker.
func TestBuildAnthropicSystemCacheDisabled(t *testing.T) {
	blocks := buildAnthropicSystem(CompletionRequest{System: []string{"a", "b", "c"}, DynamicBoundary: 2, DisableCache: true})
	if len(blocks) != 2 || blocks[0].CacheControl != nil || blocks[1].CacheControl != nil {
		t.Fatalf("want two unmarked blocks, got %+v", blocks)
	}
}
