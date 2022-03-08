package corehttp

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	cid "github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	assets "github.com/ipfs/go-ipfs/assets"
	dag "github.com/ipfs/go-merkledag"
	mfs "github.com/ipfs/go-mfs"
	path "github.com/ipfs/go-path"
	"github.com/ipfs/go-path/resolver"
	coreiface "github.com/ipfs/interface-go-ipfs-core"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	routing "github.com/libp2p/go-libp2p-core/routing"
	prometheus "github.com/prometheus/client_golang/prometheus"
)

const (
	ipfsPathPrefix        = "/ipfs/"
	ipnsPathPrefix        = "/ipns/"
	immutableCacheControl = "public, max-age=29030400, immutable"
)

var onlyAscii = regexp.MustCompile("[[:^ascii:]]")
var noModtime = time.Unix(0, 0) // disables Last-Modified header if passed as modtime

// HTML-based redirect for errors which can be recovered from, but we want
// to provide hint to people that they should fix things on their end.
var redirectTemplate = template.Must(template.New("redirect").Parse(`<!DOCTYPE html>
<html>
	<head>
		<meta charset="utf-8">
		<meta http-equiv="refresh" content="10;url={{.RedirectURL}}" />
		<link rel="canonical" href="{{.RedirectURL}}" />
	</head>
	<body>
		<pre>{{.ErrorMsg}}</pre><pre>(if a redirect does not happen in 10 seconds, use "{{.SuggestedPath}}" instead)</pre>
	</body>
</html>`))

type redirectTemplateData struct {
	RedirectURL   string
	SuggestedPath string
	ErrorMsg      string
}

// gatewayHandler is a HTTP handler that serves IPFS objects (accessible by default at /ipfs/<path>)
// (it serves requests like GET /ipfs/QmVRzPKPzNtSrEzBFm2UZfxmPAgnaLke4DMcerbsGGSaFe/link)
type gatewayHandler struct {
	config GatewayConfig
	api    coreiface.CoreAPI

	// TODO: add metrics for non-unixfs responses (block, car)
	unixfsGetMetric *prometheus.SummaryVec
}

// StatusResponseWriter enables us to override HTTP Status Code passed to
// WriteHeader function inside of http.ServeContent.  Decision is based on
// presence of HTTP Headers such as Location.
type statusResponseWriter struct {
	http.ResponseWriter
}

func (sw *statusResponseWriter) WriteHeader(code int) {
	// Check if we need to adjust Status Code to account for scheduled redirect
	// This enables us to return payload along with HTTP 301
	// for subdomain redirect in web browsers while also returning body for cli
	// tools which do not follow redirects by default (curl, wget).
	redirect := sw.ResponseWriter.Header().Get("Location")
	if redirect != "" && code == http.StatusOK {
		code = http.StatusMovedPermanently
		log.Debugw("subdomain redirect", "location", redirect, "status", code)
	}
	sw.ResponseWriter.WriteHeader(code)
}

func newGatewayHandler(c GatewayConfig, api coreiface.CoreAPI) *gatewayHandler {
	unixfsGetMetric := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "ipfs",
			Subsystem: "http",
			Name:      "unixfs_get_latency_seconds",
			Help:      "The time till the first block is received when 'getting' a file from the gateway.",
		},
		[]string{"gateway"},
	)
	if err := prometheus.Register(unixfsGetMetric); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			unixfsGetMetric = are.ExistingCollector.(*prometheus.SummaryVec)
		} else {
			log.Errorf("failed to register unixfsGetMetric: %v", err)
		}
	}

	i := &gatewayHandler{
		config:          c,
		api:             api,
		unixfsGetMetric: unixfsGetMetric,
	}
	return i
}

func parseIpfsPath(p string) (cid.Cid, string, error) {
	rootPath, err := path.ParsePath(p)
	if err != nil {
		return cid.Cid{}, "", err
	}

	// Check the path.
	rsegs := rootPath.Segments()
	if rsegs[0] != "ipfs" {
		return cid.Cid{}, "", fmt.Errorf("WritableGateway: only ipfs paths supported")
	}

	rootCid, err := cid.Decode(rsegs[1])
	if err != nil {
		return cid.Cid{}, "", err
	}

	return rootCid, path.Join(rsegs[2:]), nil
}

func (i *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// the hour is a hard fallback, we don't expect it to happen, but just in case
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	r = r.WithContext(ctx)

	defer func() {
		if r := recover(); r != nil {
			log.Error("A panic occurred in the gateway handler!")
			log.Error(r)
			debug.PrintStack()
		}
	}()

	if i.config.Writable {
		switch r.Method {
		case http.MethodPost:
			i.postHandler(w, r)
			return
		case http.MethodPut:
			i.putHandler(w, r)
			return
		case http.MethodDelete:
			i.deleteHandler(w, r)
			return
		}
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		i.getOrHeadHandler(w, r)
		return
	case http.MethodOptions:
		i.optionsHandler(w, r)
		return
	}

	errmsg := "Method " + r.Method + " not allowed: "
	var status int
	if !i.config.Writable {
		status = http.StatusMethodNotAllowed
		errmsg = errmsg + "read only access"
		w.Header().Add("Allow", http.MethodGet)
		w.Header().Add("Allow", http.MethodHead)
		w.Header().Add("Allow", http.MethodOptions)
	} else {
		status = http.StatusBadRequest
		errmsg = errmsg + "bad request for " + r.URL.Path
	}
	http.Error(w, errmsg, status)
}

func (i *gatewayHandler) optionsHandler(w http.ResponseWriter, r *http.Request) {
	/*
		OPTIONS is a noop request that is used by the browsers to check
		if server accepts cross-site XMLHttpRequest (indicated by the presence of CORS headers)
		https://developer.mozilla.org/en-US/docs/Web/HTTP/Access_control_CORS#Preflighted_requests
	*/
	i.addUserHeaders(w) // return all custom headers (including CORS ones, if set)
}

func (i *gatewayHandler) getOrHeadHandler(w http.ResponseWriter, r *http.Request) {
	begin := time.Now()
	urlPath := r.URL.Path
	escapedURLPath := r.URL.EscapedPath()

	logger := log.With("from", r.RequestURI)
	logger.Debug("http request received")

	// If the gateway is behind a reverse proxy and mounted at a sub-path,
	// the prefix header can be set to signal this sub-path.
	// It will be prepended to links in directory listings and the index.html redirect.
	// TODO: this feature is deprecated and will be removed  (https://github.com/ipfs/go-ipfs/issues/7702)
	prefix := ""
	if prfx := r.Header.Get("X-Ipfs-Gateway-Prefix"); len(prfx) > 0 {
		for _, p := range i.config.PathPrefixes {
			if prfx == p || strings.HasPrefix(prfx, p+"/") {
				prefix = prfx
				break
			}
		}
		logger.Debugw("sub-path (deprecrated)", "prefix", prefix)
	}

	// HostnameOption might have constructed an IPNS/IPFS path using the Host header.
	// In this case, we need the original path for constructing redirects
	// and links that match the requested URL.
	// For example, http://example.net would become /ipns/example.net, and
	// the redirects and links would end up as http://example.net/ipns/example.net
	requestURI, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		webError(w, "failed to parse request path", err, http.StatusInternalServerError)
		return
	}
	originalUrlPath := prefix + requestURI.Path

	// ?uri query param support for requests produced by web browsers
	// via navigator.registerProtocolHandler Web API
	// https://developer.mozilla.org/en-US/docs/Web/API/Navigator/registerProtocolHandler
	// TLDR: redirect /ipfs/?uri=ipfs%3A%2F%2Fcid%3Fquery%3Dval to /ipfs/cid?query=val
	if uriParam := r.URL.Query().Get("uri"); uriParam != "" {
		u, err := url.Parse(uriParam)
		if err != nil {
			webError(w, "failed to parse uri query parameter", err, http.StatusBadRequest)
			return
		}
		if u.Scheme != "ipfs" && u.Scheme != "ipns" {
			webError(w, "uri query parameter scheme must be ipfs or ipns", err, http.StatusBadRequest)
			return
		}
		path := u.Path
		if u.RawQuery != "" { // preserve query if present
			path = path + "?" + u.RawQuery
		}

		redirectURL := gopath.Join("/", prefix, u.Scheme, u.Host, path)
		logger.Debugw("uri param, redirect", "to", redirectURL, "status", http.StatusMovedPermanently)
		http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
		return
	}

	// Service Worker registration request
	if r.Header.Get("Service-Worker") == "script" {
		// Disallow Service Worker registration on namespace roots
		// https://github.com/ipfs/go-ipfs/issues/4025
		matched, _ := regexp.MatchString(`^/ip[fn]s/[^/]+$`, r.URL.Path)
		if matched {
			err := fmt.Errorf("registration is not allowed for this scope")
			webError(w, "navigator.serviceWorker", err, http.StatusBadRequest)
			return
		}
	}

	parsedPath := ipath.New(urlPath)
	if pathErr := parsedPath.IsValid(); pathErr != nil {
		if prefix == "" && fixupSuperfluousNamespace(w, urlPath, r.URL.RawQuery) {
			// the error was due to redundant namespace, which we were able to fix
			// by returning error/redirect page, nothing left to do here
			logger.Debugw("redundant namespace; noop")
			return
		}
		// unable to fix path, returning error
		webError(w, "invalid ipfs path", pathErr, http.StatusBadRequest)
		return
	}

	// Resolve path to the final DAG node for the ETag
	resolvedPath, err := i.api.ResolvePath(r.Context(), parsedPath)
	switch err {
	case nil:
	case coreiface.ErrOffline:
		webError(w, "ipfs resolve -r "+escapedURLPath, err, http.StatusServiceUnavailable)
		return
	default:
		if i.servePretty404IfPresent(w, r, parsedPath) {
			logger.Debugw("serve pretty 404 if present")
			return
		}

		webError(w, "ipfs resolve -r "+escapedURLPath, err, http.StatusNotFound)
		return
	}

	// Finish early if client already has matching Etag
	// (suffix match to cover both direct CID and DirIndex cases)
	cidEtagSuffix := resolvedPath.Cid().String() + `"`
	if strings.HasSuffix(r.Header.Get("If-None-Match"), cidEtagSuffix) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// HTTP Headers
	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("X-Ipfs-Path", urlPath)

	if rootCids, err := i.buildIpfsRootsHeader(urlPath, r); err == nil {
		w.Header().Set("X-Ipfs-Roots", rootCids)
	} else { // this should never happen, as we resolved the urlPath already
		webError(w, "error while resolving X-Ipfs-Roots", err, http.StatusInternalServerError)
		return
	}

	// Support custom response formats passed via ?format or Accept HTTP header
	if contentType := getExplicitContentType(r); contentType != "" {
		switch contentType {
		case "application/vnd.ipld.raw":
			logger.Debugw("serving raw block", "path", parsedPath)
			i.serveRawBlock(w, r, resolvedPath.Cid(), parsedPath)
			return
		case "application/vnd.ipld.car":
			logger.Debugw("serving car stream", "path", parsedPath)
			i.serveCar(w, r, resolvedPath.Cid(), parsedPath)
			return
		case "application/vnd.ipld.car; version=1":
			logger.Debugw("serving car stream", "path", parsedPath)
			i.serveCar(w, r, resolvedPath.Cid(), parsedPath)
			return
		case "application/vnd.ipld.car; version=2": // no CARv2 in go-ipfs atm
			err := fmt.Errorf("unsupported CARv2 format, try again with CARv1")
			webError(w, "failed respond with requested content type", err, http.StatusBadRequest)
			return
		default:
			err := fmt.Errorf("unsupported format %q", contentType)
			webError(w, "failed respond with requested content type", err, http.StatusBadRequest)
			return
		}
	}

	// Handling Unixfs
	dr, err := i.api.Unixfs().Get(r.Context(), resolvedPath)
	if err != nil {
		webError(w, "ipfs cat "+escapedURLPath, err, http.StatusNotFound)
		return
	}
	// TODO:  do we want to reuse unixfsGetMetric for block/car, or should we have separate ones?
	i.unixfsGetMetric.WithLabelValues(parsedPath.Namespace()).Observe(time.Since(begin).Seconds())
	defer dr.Close()

	// Handling Unixfs file
	if f, ok := dr.(files.File); ok {
		logger.Debugw("serving file", "path", parsedPath)
		i.serveFile(w, r, parsedPath, resolvedPath.Cid(), f)
		return
	}

	// Handling Unixfs directory
	dir, ok := dr.(files.Directory)
	if !ok {
		internalWebError(w, fmt.Errorf("unsupported file type"))
		return
	}

	// Check if directory has index.html, if so, serveFile
	idxPath := ipath.Join(resolvedPath, "index.html")
	idx, err := i.api.Unixfs().Get(r.Context(), idxPath)
	switch err.(type) {
	case nil:
		dirwithoutslash := urlPath[len(urlPath)-1] != '/'
		goget := r.URL.Query().Get("go-get") == "1"
		if dirwithoutslash && !goget {
			// See comment above where originalUrlPath is declared.
			suffix := "/"
			if r.URL.RawQuery != "" {
				// preserve query parameters
				suffix = suffix + "?" + r.URL.RawQuery
			}

			redirectURL := originalUrlPath + suffix
			logger.Debugw("serving index.html file", "to", redirectURL, "status", http.StatusFound, "path", idxPath)
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}

		f, ok := idx.(files.File)
		if !ok {
			internalWebError(w, files.ErrNotReader)
			return
		}

		logger.Debugw("serving index.html file", "path", idxPath)
		// write to request
		i.serveFile(w, r, idxPath, resolvedPath.Cid(), f)
		return
	case resolver.ErrNoLink:
		logger.Debugw("no index.html; noop", "path", idxPath)
	default:
		internalWebError(w, err)
		return
	}

	// See statusResponseWriter.WriteHeader
	// and https://github.com/ipfs/go-ipfs/issues/7164
	// Note: this needs to occur before listingTemplate.Execute otherwise we get
	// superfluous response.WriteHeader call from prometheus/client_golang
	if w.Header().Get("Location") != "" {
		logger.Debugw("location moved permanently", "status", http.StatusMovedPermanently)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}

	// A HTML directory index will be presented, be sure to set the correct
	// type instead of relying on autodetection (which may fail).
	w.Header().Set("Content-Type", "text/html")

	// Generated dir index requires custom Etag (it may change between go-ipfs versions)
	if assets.BindataVersionHash != "" {
		dirEtag := `"DirIndex-` + assets.BindataVersionHash + `_CID-` + resolvedPath.Cid().String() + `"`
		w.Header().Set("Etag", dirEtag)
		if r.Header.Get("If-None-Match") == dirEtag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	if r.Method == http.MethodHead {
		logger.Debug("return as request's HTTP method is HEAD")
		return
	}

	// storage for directory listing
	var dirListing []directoryItem
	dirit := dir.Entries()
	for dirit.Next() {
		size := "?"
		if s, err := dirit.Node().Size(); err == nil {
			// Size may not be defined/supported. Continue anyways.
			size = humanize.Bytes(uint64(s))
		}

		resolved, err := i.api.ResolvePath(r.Context(), ipath.Join(resolvedPath, dirit.Name()))
		if err != nil {
			internalWebError(w, err)
			return
		}
		hash := resolved.Cid().String()

		// See comment above where originalUrlPath is declared.
		di := directoryItem{
			Size:      size,
			Name:      dirit.Name(),
			Path:      gopath.Join(originalUrlPath, dirit.Name()),
			Hash:      hash,
			ShortHash: shortHash(hash),
		}
		dirListing = append(dirListing, di)
	}
	if dirit.Err() != nil {
		internalWebError(w, dirit.Err())
		return
	}

	// construct the correct back link
	// https://github.com/ipfs/go-ipfs/issues/1365
	var backLink string = originalUrlPath

	// don't go further up than /ipfs/$hash/
	pathSplit := path.SplitList(urlPath)
	switch {
	// keep backlink
	case len(pathSplit) == 3: // url: /ipfs/$hash

	// keep backlink
	case len(pathSplit) == 4 && pathSplit[3] == "": // url: /ipfs/$hash/

	// add the correct link depending on whether the path ends with a slash
	default:
		if strings.HasSuffix(backLink, "/") {
			backLink += "./.."
		} else {
			backLink += "/.."
		}
	}

	size := "?"
	if s, err := dir.Size(); err == nil {
		// Size may not be defined/supported. Continue anyways.
		size = humanize.Bytes(uint64(s))
	}

	hash := resolvedPath.Cid().String()

	// Gateway root URL to be used when linking to other rootIDs.
	// This will be blank unless subdomain or DNSLink resolution is being used
	// for this request.
	var gwURL string

	// Get gateway hostname and build gateway URL.
	if h, ok := r.Context().Value("gw-hostname").(string); ok {
		gwURL = "//" + h
	} else {
		gwURL = ""
	}

	dnslink := hasDNSLinkOrigin(gwURL, urlPath)

	// See comment above where originalUrlPath is declared.
	tplData := listingTemplateData{
		GatewayURL:  gwURL,
		DNSLink:     dnslink,
		Listing:     dirListing,
		Size:        size,
		Path:        urlPath,
		Breadcrumbs: breadcrumbs(urlPath, dnslink),
		BackLink:    backLink,
		Hash:        hash,
	}

	logger.Debugw("request processed", "tplDataDNSLink", dnslink, "tplDataSize", size, "tplDataBackLink", backLink, "tplDataHash", hash, "duration", time.Since(begin))

	if err := listingTemplate.Execute(w, tplData); err != nil {
		internalWebError(w, err)
		return
	}
}

func (i *gatewayHandler) servePretty404IfPresent(w http.ResponseWriter, r *http.Request, parsedPath ipath.Path) bool {
	resolved404Path, ctype, err := i.searchUpTreeFor404(r, parsedPath)
	if err != nil {
		return false
	}

	dr, err := i.api.Unixfs().Get(r.Context(), resolved404Path)
	if err != nil {
		return false
	}
	defer dr.Close()

	f, ok := dr.(files.File)
	if !ok {
		return false
	}

	size, err := f.Size()
	if err != nil {
		return false
	}

	log.Debugw("using pretty 404 file", "path", parsedPath)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusNotFound)
	_, err = io.CopyN(w, f, size)
	return err == nil
}

func (i *gatewayHandler) postHandler(w http.ResponseWriter, r *http.Request) {
	p, err := i.api.Unixfs().Add(r.Context(), files.NewReaderFile(r.Body))
	if err != nil {
		internalWebError(w, err)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", p.Cid().String())
	log.Debugw("CID created, http redirect", "from", r.URL, "to", p, "status", http.StatusCreated)
	http.Redirect(w, r, p.String(), http.StatusCreated)
}

func (i *gatewayHandler) putHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds := i.api.Dag()

	// Parse the path
	rootCid, newPath, err := parseIpfsPath(r.URL.Path)
	if err != nil {
		webError(w, "WritableGateway: failed to parse the path", err, http.StatusBadRequest)
		return
	}
	if newPath == "" || newPath == "/" {
		http.Error(w, "WritableGateway: empty path", http.StatusBadRequest)
		return
	}
	newDirectory, newFileName := gopath.Split(newPath)

	// Resolve the old root.

	rnode, err := ds.Get(ctx, rootCid)
	if err != nil {
		webError(w, "WritableGateway: Could not create DAG from request", err, http.StatusInternalServerError)
		return
	}

	pbnd, ok := rnode.(*dag.ProtoNode)
	if !ok {
		webError(w, "Cannot read non protobuf nodes through gateway", dag.ErrNotProtobuf, http.StatusBadRequest)
		return
	}

	// Create the new file.
	newFilePath, err := i.api.Unixfs().Add(ctx, files.NewReaderFile(r.Body))
	if err != nil {
		webError(w, "WritableGateway: could not create DAG from request", err, http.StatusInternalServerError)
		return
	}

	newFile, err := ds.Get(ctx, newFilePath.Cid())
	if err != nil {
		webError(w, "WritableGateway: failed to resolve new file", err, http.StatusInternalServerError)
		return
	}

	// Patch the new file into the old root.

	root, err := mfs.NewRoot(ctx, ds, pbnd, nil)
	if err != nil {
		webError(w, "WritableGateway: failed to create MFS root", err, http.StatusBadRequest)
		return
	}

	if newDirectory != "" {
		err := mfs.Mkdir(root, newDirectory, mfs.MkdirOpts{Mkparents: true, Flush: false})
		if err != nil {
			webError(w, "WritableGateway: failed to create MFS directory", err, http.StatusInternalServerError)
			return
		}
	}
	dirNode, err := mfs.Lookup(root, newDirectory)
	if err != nil {
		webError(w, "WritableGateway: failed to lookup directory", err, http.StatusInternalServerError)
		return
	}
	dir, ok := dirNode.(*mfs.Directory)
	if !ok {
		http.Error(w, "WritableGateway: target directory is not a directory", http.StatusBadRequest)
		return
	}
	err = dir.Unlink(newFileName)
	switch err {
	case os.ErrNotExist, nil:
	default:
		webError(w, "WritableGateway: failed to replace existing file", err, http.StatusBadRequest)
		return
	}
	err = dir.AddChild(newFileName, newFile)
	if err != nil {
		webError(w, "WritableGateway: failed to link file into directory", err, http.StatusInternalServerError)
		return
	}
	nnode, err := root.GetDirectory().GetNode()
	if err != nil {
		webError(w, "WritableGateway: failed to finalize", err, http.StatusInternalServerError)
		return
	}
	newcid := nnode.Cid()

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", newcid.String())

	redirectURL := gopath.Join(ipfsPathPrefix, newcid.String(), newPath)
	log.Debugw("CID replaced, redirect", "from", r.URL, "to", redirectURL, "status", http.StatusCreated)
	http.Redirect(w, r, redirectURL, http.StatusCreated)
}

func (i *gatewayHandler) deleteHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// parse the path

	rootCid, newPath, err := parseIpfsPath(r.URL.Path)
	if err != nil {
		webError(w, "WritableGateway: failed to parse the path", err, http.StatusBadRequest)
		return
	}
	if newPath == "" || newPath == "/" {
		http.Error(w, "WritableGateway: empty path", http.StatusBadRequest)
		return
	}
	directory, filename := gopath.Split(newPath)

	// lookup the root

	rootNodeIPLD, err := i.api.Dag().Get(ctx, rootCid)
	if err != nil {
		webError(w, "WritableGateway: failed to resolve root CID", err, http.StatusInternalServerError)
		return
	}
	rootNode, ok := rootNodeIPLD.(*dag.ProtoNode)
	if !ok {
		http.Error(w, "WritableGateway: empty path", http.StatusInternalServerError)
		return
	}

	// construct the mfs root

	root, err := mfs.NewRoot(ctx, i.api.Dag(), rootNode, nil)
	if err != nil {
		webError(w, "WritableGateway: failed to construct the MFS root", err, http.StatusBadRequest)
		return
	}

	// lookup the parent directory

	parentNode, err := mfs.Lookup(root, directory)
	if err != nil {
		webError(w, "WritableGateway: failed to look up parent", err, http.StatusInternalServerError)
		return
	}

	parent, ok := parentNode.(*mfs.Directory)
	if !ok {
		http.Error(w, "WritableGateway: parent is not a directory", http.StatusInternalServerError)
		return
	}

	// delete the file

	switch parent.Unlink(filename) {
	case nil, os.ErrNotExist:
	default:
		webError(w, "WritableGateway: failed to remove file", err, http.StatusInternalServerError)
		return
	}

	nnode, err := root.GetDirectory().GetNode()
	if err != nil {
		webError(w, "WritableGateway: failed to finalize", err, http.StatusInternalServerError)
	}
	ncid := nnode.Cid()

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", ncid.String())

	redirectURL := gopath.Join(ipfsPathPrefix+ncid.String(), directory)
	// note: StatusCreated is technically correct here as we created a new resource.
	log.Debugw("CID deleted, redirect", "from", r.RequestURI, "to", redirectURL, "status", http.StatusCreated)
	http.Redirect(w, r, redirectURL, http.StatusCreated)
}

func (i *gatewayHandler) addUserHeaders(w http.ResponseWriter) {
	for k, v := range i.config.Headers {
		w.Header()[k] = v
	}
}

func addCacheControlHeaders(w http.ResponseWriter, r *http.Request, contentPath ipath.Path, fileCid cid.Cid) (modtime time.Time) {
	// Set Etag to file's CID (override whatever was set before)
	w.Header().Set("Etag", `"`+fileCid.String()+`"`)

	// Set Cache-Control and Last-Modified based on contentPath properties
	if contentPath.Mutable() {
		// mutable namespaces such as /ipns/ can't be cached forever

		/* For now we set Last-Modified to Now() to leverage caching heuristics built into modern browsers:
		 * https://github.com/ipfs/go-ipfs/pull/8074#pullrequestreview-645196768
		 * but we should not set it to fake values and use Cache-Control based on TTL instead */
		modtime = time.Now()

		// TODO: set Cache-Control based on TTL of IPNS/DNSLink: https://github.com/ipfs/go-ipfs/issues/1818#issuecomment-1015849462
		// TODO: set Last-Modified if modification metadata is present in unixfs 1.5: https://github.com/ipfs/go-ipfs/issues/6920

	} else {
		// immutable! CACHE ALL THE THINGS, FOREVER! wolololol
		w.Header().Set("Cache-Control", immutableCacheControl)

		// Set modtime to 'zero time' to disable Last-Modified header (superseded by Cache-Control)
		modtime = noModtime

		// TODO: set Last-Modified if modification metadata is present in unixfs 1.5: https://github.com/ipfs/go-ipfs/issues/6920
	}

	return modtime
}

// Set Content-Disposition if filename URL query param is present, return preferred filename
func addContentDispositionHeader(w http.ResponseWriter, r *http.Request, contentPath ipath.Path) string {
	/* This logic enables:
	 * - creation of HTML links that trigger "Save As.." dialog instead of being rendered by the browser
	 * - overriding the filename used when saving subresource assets on HTML page
	 * - providing a default filename for HTTP clients when downloading direct /ipfs/CID without any subpath
	 */

	// URL param ?filename=cat.jpg triggers Content-Disposition: [..] filename
	// which impacts default name used in "Save As.." dialog
	name := getFilename(contentPath)
	urlFilename := r.URL.Query().Get("filename")
	if urlFilename != "" {
		disposition := "inline"
		// URL param ?download=true triggers Content-Disposition: [..] attachment
		// which skips rendering and forces "Save As.." dialog in browsers
		if r.URL.Query().Get("download") == "true" {
			disposition = "attachment"
		}
		setContentDispositionHeader(w, urlFilename, disposition)
		name = urlFilename
	}
	return name
}

// Set Content-Disposition to arbitrary filename and disposition
func setContentDispositionHeader(w http.ResponseWriter, filename string, disposition string) {
	utf8Name := url.PathEscape(filename)
	asciiName := url.PathEscape(onlyAscii.ReplaceAllLiteralString(filename, "_"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s", disposition, asciiName, utf8Name))
}

// Set X-Ipfs-Roots with logical CID array for efficient HTTP cache invalidation.
func (i *gatewayHandler) buildIpfsRootsHeader(contentPath string, r *http.Request) (string, error) {
	/*
		These are logical roots where each CID represent one path segment
		and resolves to either a directory or the root block of a file.
		The main purpose of this header is allow HTTP caches to do smarter decisions
		around cache invalidation (eg. keep specific subdirectory/file if it did not change)

		A good example is Wikipedia, which is HAMT-sharded, but we only care about
		logical roots that represent each segment of the human-readable content
		path:

		Given contentPath = /ipns/en.wikipedia-on-ipfs.org/wiki/Block_of_Wikipedia_in_Turkey
		rootCidList is a generated by doing `ipfs resolve -r` on each sub path:
			/ipns/en.wikipedia-on-ipfs.org → bafybeiaysi4s6lnjev27ln5icwm6tueaw2vdykrtjkwiphwekaywqhcjze
			/ipns/en.wikipedia-on-ipfs.org/wiki/ → bafybeihn2f7lhumh4grizksi2fl233cyszqadkn424ptjajfenykpsaiw4
			/ipns/en.wikipedia-on-ipfs.org/wiki/Block_of_Wikipedia_in_Turkey → bafkreibn6euazfvoghepcm4efzqx5l3hieof2frhp254hio5y7n3hv5rma

		The result is an ordered array of values:
			X-Ipfs-Roots: bafybeiaysi4s6lnjev27ln5icwm6tueaw2vdykrtjkwiphwekaywqhcjze,bafybeihn2f7lhumh4grizksi2fl233cyszqadkn424ptjajfenykpsaiw4,bafkreibn6euazfvoghepcm4efzqx5l3hieof2frhp254hio5y7n3hv5rma

		Note that while the top one will change every time any article is changed,
		the last root (responsible for specific article) may not change at all.
	*/
	var sp strings.Builder
	var pathRoots []string
	pathSegments := strings.Split(contentPath[6:], "/")
	sp.WriteString(contentPath[:5]) // /ipfs or /ipns
	for _, root := range pathSegments {
		if root == "" {
			continue
		}
		sp.WriteString("/")
		sp.WriteString(root)
		resolvedSubPath, err := i.api.ResolvePath(r.Context(), ipath.New(sp.String()))
		if err != nil {
			return "", err
		}
		pathRoots = append(pathRoots, resolvedSubPath.Cid().String())
	}
	rootCidList := strings.Join(pathRoots, ",") // convention from rfc2616#sec4.2
	return rootCidList, nil
}

func webError(w http.ResponseWriter, message string, err error, defaultCode int) {
	if _, ok := err.(resolver.ErrNoLink); ok {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == routing.ErrNotFound {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == context.DeadlineExceeded {
		webErrorWithCode(w, message, err, http.StatusRequestTimeout)
	} else {
		webErrorWithCode(w, message, err, defaultCode)
	}
}

func webErrorWithCode(w http.ResponseWriter, message string, err error, code int) {
	http.Error(w, fmt.Sprintf("%s: %s", message, err), code)
	if code >= 500 {
		log.Warnf("server error: %s: %s", err)
	}
}

// return a 500 error and log
func internalWebError(w http.ResponseWriter, err error) {
	webErrorWithCode(w, "internalWebError", err, http.StatusInternalServerError)
}

func getFilename(contentPath ipath.Path) string {
	s := contentPath.String()
	if (strings.HasPrefix(s, ipfsPathPrefix) || strings.HasPrefix(s, ipnsPathPrefix)) && strings.Count(gopath.Clean(s), "/") <= 2 {
		// Don't want to treat ipfs.io in /ipns/ipfs.io as a filename.
		return ""
	}
	return gopath.Base(s)
}

// return explicit response format if specified in request as query parameter or via Accept HTTP header
func getExplicitContentType(r *http.Request) string {
	if formatParam := r.URL.Query().Get("format"); formatParam != "" {
		// translate query param to a content type
		switch formatParam {
		case "raw":
			return "application/vnd.ipld.raw"
		case "car":
			return "application/vnd.ipld.car"
		}
	}
	if accept := r.Header.Get("Accept"); strings.HasPrefix(accept, "application/vnd.") {
		return accept
	}
	return ""
}

func (i *gatewayHandler) searchUpTreeFor404(r *http.Request, parsedPath ipath.Path) (ipath.Resolved, string, error) {
	filename404, ctype, err := preferred404Filename(r.Header.Values("Accept"))
	if err != nil {
		return nil, "", err
	}

	pathComponents := strings.Split(parsedPath.String(), "/")

	for idx := len(pathComponents); idx >= 3; idx-- {
		pretty404 := gopath.Join(append(pathComponents[0:idx], filename404)...)
		parsed404Path := ipath.New("/" + pretty404)
		if parsed404Path.IsValid() != nil {
			break
		}
		resolvedPath, err := i.api.ResolvePath(r.Context(), parsed404Path)
		if err != nil {
			continue
		}
		return resolvedPath, ctype, nil
	}

	return nil, "", fmt.Errorf("no pretty 404 in any parent folder")
}

func preferred404Filename(acceptHeaders []string) (string, string, error) {
	// If we ever want to offer a 404 file for a different content type
	// then this function will need to parse q weightings, but for now
	// the presence of anything matching HTML is enough.
	for _, acceptHeader := range acceptHeaders {
		accepted := strings.Split(acceptHeader, ",")
		for _, spec := range accepted {
			contentType := strings.SplitN(spec, ";", 1)[0]
			switch contentType {
			case "*/*", "text/*", "text/html":
				return "ipfs-404.html", "text/html", nil
			}
		}
	}

	return "", "", fmt.Errorf("there is no 404 file for the requested content types")
}

// Attempt to fix redundant /ipfs/ namespace as long as resulting
// 'intended' path is valid.  This is in case gremlins were tickled
// wrong way and user ended up at /ipfs/ipfs/{cid} or /ipfs/ipns/{id}
// like in bafybeien3m7mdn6imm425vc2s22erzyhbvk5n3ofzgikkhmdkh5cuqbpbq :^))
func fixupSuperfluousNamespace(w http.ResponseWriter, urlPath string, urlQuery string) bool {
	if !(strings.HasPrefix(urlPath, "/ipfs/ipfs/") || strings.HasPrefix(urlPath, "/ipfs/ipns/")) {
		return false // not a superfluous namespace
	}
	intendedPath := ipath.New(strings.TrimPrefix(urlPath, "/ipfs"))
	if err := intendedPath.IsValid(); err != nil {
		return false // not a valid path
	}
	intendedURL := intendedPath.String()
	if urlQuery != "" {
		// we render HTML, so ensure query entries are properly escaped
		q, _ := url.ParseQuery(urlQuery)
		intendedURL = intendedURL + "?" + q.Encode()
	}
	// return HTTP 400 (Bad Request) with HTML error page that:
	// - points at correct canonical path via <link> header
	// - displays human-readable error
	// - redirects to intendedURL after a short delay
	w.WriteHeader(http.StatusBadRequest)
	return redirectTemplate.Execute(w, redirectTemplateData{
		RedirectURL:   intendedURL,
		SuggestedPath: intendedPath.String(),
		ErrorMsg:      fmt.Sprintf("invalid path: %q should be %q", urlPath, intendedPath.String()),
	}) == nil
}
