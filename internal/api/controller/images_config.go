package controller

import (
	"encoding/json"
	"log"
	"os"
)

const envImagesConfig = "HIVER_IMAGES_CONFIG"

// imageEntry maps a logical image name to a Docker image reference and, when
// set, whether the host runs in pack/prewarm mode for it. When pack is omitted,
// the top-level config.Pack default applies (which itself defaults true,
// design §11), so a bare {"ref": ...} entry inherits the file-wide setting.
type imageEntry struct {
	Ref  string `json:"ref"`
	Pack *bool  `json:"pack"`
}

// config is the images config carried in HIVER_IMAGES_CONFIG. Pack is the
// file-wide pack default (true when omitted); Images maps logical names to refs.
type config struct {
	Pack   *bool                 `json:"pack"`
	Images map[string]imageEntry `json:"images"`
}

func (c config) packDefault() bool {
	if c.Pack != nil {
		return *c.Pack
	}
	return true
}

func (c config) packFor(e imageEntry) bool {
	if e.Pack != nil {
		return *e.Pack
	}
	return c.packDefault()
}

// loadImagesConfig parses the images config JSON in HIVER_IMAGES_CONFIG. An
// unset/empty value yields an empty config (not an error): the docker runtime
// then falls back to treating image names as full refs and to the default image.
// A malformed value is logged and treated as empty so a typo can't take the
// controller down.
func loadImagesConfig() config {
	empty := config{Images: map[string]imageEntry{}}
	raw := os.Getenv(envImagesConfig)
	if raw == "" {
		return empty
	}
	var cfg config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		log.Printf("controller: parse %s: %v", envImagesConfig, err)
		return empty
	}
	if cfg.Images == nil {
		cfg.Images = map[string]imageEntry{}
	}
	log.Printf("controller: loaded %d image(s) from %s (pack default %t)", len(cfg.Images), envImagesConfig, cfg.packDefault())
	return cfg
}

// resolveImage maps a logical image name (or a full Docker ref) onto the ref to
// run and whether to run it in pack mode. A name present in the config uses its
// ref and pack flag (per-entry, else the file-wide default). An empty name uses
// the default image; an unmapped name is treated as a full ref.
func resolveImage(c config, name string) (ref string, pack bool) {
	// An empty name selects the default logical image so it resolves through the
	// config (picking up the right container/microvm ref and pack flag).
	if name == "" {
		name = defaultImageName
	}
	if e, ok := c.Images[name]; ok {
		return e.Ref, c.packFor(e)
	}
	return name, c.packDefault()
}
