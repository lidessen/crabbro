package inspect

import (
	"runtime"
	"sync"
	"time"
)

// Catalog is the in-memory source and page index for this daemon process.
type Catalog struct {
	mu      sync.Mutex
	nextSrc int
	nextPg  int
	sources map[string]*Source
	byKey   map[string]*Source
	pages   map[string]*Page
	pgKey   map[string]*Page
}

func NewCatalog() *Catalog {
	return &Catalog{
		sources: map[string]*Source{},
		byKey:   map[string]*Source{},
		pages:   map[string]*Page{},
		pgKey:   map[string]*Page{},
	}
}

// Process is the OS process that owns the client socket.
type Process struct {
	PID        int
	HelperPID  int
	Name       string
	Executable string
	Kind       string
	Platform   string
}

// Ensure returns the source and page for this process and class without counting.
func (c *Catalog) Ensure(proc Process, class Class) (Source, Page) {
	return c.observe(proc, class, nil)
}

// Tally records one exchange against an existing source and page.
func (c *Catalog) Tally(sourceID, pageID string, verdict Verdict) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	src := c.sources[sourceID]
	pg := c.pages[pageID]
	now := time.Now().UTC()
	if src != nil {
		src.LastSeen = now
		src.Seen++
		if verdict.Hidden {
			src.Hidden++
		} else if verdict.InScope {
			src.InScope++
		}
	}
	if pg != nil {
		pg.LastSeen = now
		pg.Seen++
		if verdict.Hidden {
			pg.Hidden++
		} else if verdict.InScope {
			pg.InScope++
		}
	}
}

func (c *Catalog) Observe(proc Process, class Class, verdict Verdict) (Source, Page) {
	return c.observe(proc, class, &verdict)
}

func (c *Catalog) observe(proc Process, class Class, verdict *Verdict) (Source, Page) {
	if c == nil {
		return Source{}, Page{}
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if proc.Platform == "" {
		proc.Platform = runtime.GOOS
	}
	if proc.Kind == "" {
		proc.Kind = "unknown"
	}
	key := proc.Executable
	if key == "" {
		key = proc.Kind + ":" + proc.Name
	}
	if proc.Kind == "unknown" {
		key = "unknown"
	}
	src := c.byKey[key]
	if src == nil {
		c.nextSrc++
		src = &Source{
			ID:         "src_" + itoa(c.nextSrc),
			Kind:       proc.Kind,
			Name:       proc.Name,
			Executable: proc.Executable,
			Platform:   proc.Platform,
		}
		if src.Name == "" {
			src.Name = "unknown"
		}
		c.byKey[key] = src
		c.sources[src.ID] = src
	}
	src.PID = proc.PID
	src.HelperPID = proc.HelperPID
	src.Kind = proc.Kind
	src.LastSeen = now
	if verdict != nil {
		src.Seen++
		if verdict.Hidden {
			src.Hidden++
		} else if verdict.InScope {
			src.InScope++
		}
	}

	pgKey := src.ID + "|" + class.Origin + "|" + class.PageURL
	pg := c.pgKey[pgKey]
	if pg == nil {
		c.nextPg++
		pg = &Page{
			ID:         "page_" + itoa(c.nextPg),
			SourceID:   src.ID,
			Origin:     class.Origin,
			URL:        class.PageURL,
			Confidence: class.Confidence,
		}
		c.pgKey[pgKey] = pg
		c.pages[pg.ID] = pg
		src.PageIDs = append(src.PageIDs, pg.ID)
	}
	pg.LastSeen = now
	if verdict != nil {
		pg.Seen++
		if verdict.Hidden {
			pg.Hidden++
		} else if verdict.InScope {
			pg.InScope++
		}
	}
	return *src, *pg
}

func (c *Catalog) List() []Source {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Source, 0, len(c.sources))
	for _, s := range c.sources {
		cp := *s
		cp.PageIDs = append([]string(nil), s.PageIDs...)
		out = append(out, cp)
	}
	return out
}

func (c *Catalog) Get(id string) (Source, []Page, bool) {
	if c == nil {
		return Source{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sources[id]
	if !ok {
		return Source{}, nil, false
	}
	cp := *s
	var pages []Page
	for _, pid := range s.PageIDs {
		if p, ok := c.pages[pid]; ok {
			pages = append(pages, *p)
		}
	}
	return cp, pages, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
