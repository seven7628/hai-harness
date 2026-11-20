# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

rep('''	sig := h.sign(info.Id, info.Version)
	base := "http://" + h.addr + artifactPreviewPrefix +
		url.PathEscape(info.Id) + "/" + url.PathEscape(info.Version) + "/" + sig + "/"
	h.refsMu.Lock()
	h.refs[info.Id+"\\x00"+info.Version] = artifactPageRef{
		Workspace: ws, SessionID: sid, ArtifactID: info.Id, Version: info.Version,
	}
	h.refsMu.Unlock()
	return base + escapeArtifactPath(info.Entry), nil
}''','''	// 路径里必须带 wsKey：artifact id 与版本 id 都**不是全局唯一**的（两个工作区可以有
	// 同名的 artifacts/index.html，内容相同时版本 id 也相同）。只按 id+version 建索引
	// 会让后打开的 workspace 覆盖前一个 —— External 大文件会从**另一个工作区**读。
	k := wsKey(ws)
	sig := h.sign(k, info.Id, info.Version)
	base := "http://" + h.addr + artifactPreviewPrefix + k + "/" +
		url.PathEscape(info.Id) + "/" + url.PathEscape(info.Version) + "/" + sig + "/"
	h.refsMu.Lock()
	h.refs[refKey(k, info.Id, info.Version)] = artifactPageRef{
		Workspace: ws, SessionID: sid, ArtifactID: info.Id, Version: info.Version,
	}
	h.refsMu.Unlock()
	return base + escapeArtifactPath(info.Entry), nil
}

// wsKey 工作区的公开短键（8 hex）：只用于 URL 命名空间与签名，不暴露路径。
func wsKey(ws string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(ws)))
	return hex.EncodeToString(sum[:4])
}

// refKey 预览页索引键：wsKey + artifact + version，三者缺一都会串工作区。
func refKey(k, id, version string) string { return k + "\\x00" + id + "\\x00" + version }''')

rep('''func (h *artifactPreviewHost) sign(id, version string) string {
	mac := hmac.New(sha256.New, h.secret[:])
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(version))''','''func (h *artifactPreviewHost) sign(wsKey, id, version string) string {
	mac := hmac.New(sha256.New, h.secret[:])
	_, _ = mac.Write([]byte(wsKey))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(version))''')

rep('''func (h *artifactPreviewHost) signOK(id, version, sig string) bool {
	want := h.sign(id, version)''','''func (h *artifactPreviewHost) signOK(wsKey, id, version, sig string) bool {
	want := h.sign(wsKey, id, version)''')

rep('''	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	id, version, sig, tail := parts[0], parts[1], parts[2], parts[3]
	if !h.signOK(id, version, sig) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(tail, artifactAuxPrefix) {
		h.serveAux(w, r, id, version, strings.TrimPrefix(tail, artifactAuxPrefix))
		return
	}
	h.serveFile(w, r, id, version, tail)''','''	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 5 {
		http.NotFound(w, r)
		return
	}
	k, id, version, sig, tail := parts[0], parts[1], parts[2], parts[3], parts[4]
	if !h.signOK(k, id, version, sig) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(tail, artifactAuxPrefix) {
		h.serveAux(w, r, k, id, version, strings.TrimPrefix(tail, artifactAuxPrefix))
		return
	}
	h.serveFile(w, r, k, id, version, tail)''')

rep('''func (h *artifactPreviewHost) lookupRef(id, version string) artifactPageRef {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	return h.refs[id+"\\x00"+version]
}''','''func (h *artifactPreviewHost) lookupRef(k, id, version string) artifactPageRef {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	return h.refs[refKey(k, id, version)]
}''')

rep('''func artifactPageBase(id, version, sig, entry string) string {''','''func artifactPageBase(k, id, version, sig, entry string) string {''')
rep('''	return artifactPreviewPrefix + url.PathEscape(id) + "/" + url.PathEscape(version) + "/" + sig + "/" + dir
}''','''	return artifactPreviewPrefix + k + "/" + url.PathEscape(id) + "/" + url.PathEscape(version) + "/" + sig + "/" + dir
}''')
rep('''// 否则嵌套入口的 inspector 会请求到 /p/id/ver/sig/sub/~hai/... 而 404。
func artifactAuxBase(id, version, sig string) string {
	return artifactPreviewPrefix + url.PathEscape(id) + "/" + url.PathEscape(version) + "/" + sig + "/" + artifactAuxPrefix
}''','''// 否则嵌套入口的 inspector 会请求到 /p/k/id/ver/sig/sub/~hai/... 而 404。
func artifactAuxBase(k, id, version, sig string) string {
	return artifactPreviewPrefix + k + "/" + url.PathEscape(id) + "/" + url.PathEscape(version) + "/" + sig + "/" + artifactAuxPrefix
}''')

rep('''func (h *artifactPreviewHost) serveFile(w http.ResponseWriter, r *http.Request, id, version, tail string) {''',
    '''func (h *artifactPreviewHost) serveFile(w http.ResponseWriter, r *http.Request, k, id, version, tail string) {''')
rep('''	ref := h.lookupRef(id, version)
	if ref.Workspace == "" {
		http.Error(w, "unknown preview", http.StatusNotFound)
		return
	}
	v, err := h.svc.Version(ref.Workspace, id, version)''','''	ref := h.lookupRef(k, id, version)
	if ref.Workspace == "" {
		http.Error(w, "unknown preview", http.StatusNotFound)
		return
	}
	v, err := h.svc.Version(ref.Workspace, id, version)''')
rep('''		sig := h.sign(id, version)
		data = injectArtifactHTML(data,
			artifactPageBase(id, version, sig, meta.Path),
			artifactAuxBase(id, version, sig),
			id, version, sig)''','''		sig := h.sign(k, id, version)
		data = injectArtifactHTML(data,
			artifactPageBase(k, id, version, sig, meta.Path),
			artifactAuxBase(k, id, version, sig),
			id, version, sig)''')

rep('''func (h *artifactPreviewHost) serveAux(w http.ResponseWriter, r *http.Request, id, version, tail string) {''',
    '''func (h *artifactPreviewHost) serveAux(w http.ResponseWriter, r *http.Request, k, id, version, tail string) {''')
rep('''func (h *artifactPreviewHost) serveSSE(w http.ResponseWriter, r *http.Request, id string) {''',
    '''func (h *artifactPreviewHost) serveSSE(w http.ResponseWriter, r *http.Request, k, id string) {''')
rep('''func (h *artifactPreviewHost) serveMsg(w http.ResponseWriter, r *http.Request, id, version string) {''',
    '''func (h *artifactPreviewHost) serveMsg(w http.ResponseWriter, r *http.Request, k, id, version string) {''')
rep('''	ref := h.lookupRef(id, version)
	if ref.Workspace == "" {
		http.Error(w, "unknown preview", http.StatusNotFound)
		return
	}
	if h.onMsg == nil {''','''	ref := h.lookupRef(k, id, version)
	if ref.Workspace == "" {
		http.Error(w, "unknown preview", http.StatusNotFound)
		return
	}
	if h.onMsg == nil {''')

open(p,'w').write(s)
print('ok')
