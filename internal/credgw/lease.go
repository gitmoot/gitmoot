package credgw

// Lease is one job's handle on a gateway registration: its URL, placeholder
// credential and, for remote jobs, mTLS material. Revoke ends it.
type Lease struct {
	gateway     *Gateway
	placeholder string
	route       string
	capability  string
	remote      RemoteMaterial
}

func (l *Lease) URL() string {
	if l == nil || l.gateway == nil || l.route == "" {
		return ""
	}
	url := l.gateway.URL() + l.route
	if l.capability != "" {
		url += "/" + l.capability
	}
	return url
}

func (l *Lease) Placeholder() string {
	if l == nil {
		return ""
	}
	return l.placeholder
}

// RemoteMaterial returns a defensive copy of the job-scoped mTLS bundle. A
// zero value means this is a loopback-only lease.
func (l *Lease) RemoteMaterial() RemoteMaterial {
	if l == nil {
		return RemoteMaterial{}
	}
	material := l.remote
	material.CACertificate = append([]byte(nil), material.CACertificate...)
	material.ClientCertificate = append([]byte(nil), material.ClientCertificate...)
	material.ClientPrivateKey = append([]byte(nil), material.ClientPrivateKey...)
	return material
}

func (l *Lease) Revoke() {
	if l != nil && l.gateway != nil {
		if l.route != "" {
			l.gateway.revokeProxy(l.route, l.placeholder)
		} else {
			l.gateway.Revoke(l.placeholder)
		}
	}
}
