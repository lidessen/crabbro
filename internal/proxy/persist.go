package proxy

import "github.com/lidessen/crabbro/internal/shared"

func (p *Proxy) persist(req *shared.Request) {
	if p == nil || p.History == nil || req == nil || req.ID == "" {
		return
	}
	var rb, sb []byte
	if p.BodyCache != nil {
		if b := p.BodyCache.Get(req.RequestBodyRef); len(b) > 0 {
			rb = append([]byte(nil), b...)
		}
		if b := p.BodyCache.Get(req.ResponseBodyRef); len(b) > 0 {
			sb = append([]byte(nil), b...)
		}
	}
	_ = p.History.Save(req, rb, sb)
}
