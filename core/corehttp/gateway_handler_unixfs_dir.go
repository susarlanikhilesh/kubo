package corehttp

import (
	"context"
	"net/http"
	"net/url"
	gopath "path"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	cid "github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	format "github.com/ipfs/go-ipld-format"
	merkledag "github.com/ipfs/go-merkledag"
	path "github.com/ipfs/go-path"
	"github.com/ipfs/go-path/resolver"
	"github.com/ipfs/go-unixfs"
	"github.com/ipfs/go-unixfs/hamt"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/ipfs/kubo/assets"
	"github.com/ipfs/kubo/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// serveDirectory returns the best representation of UnixFS directory
//
// It will return index.html if present, or generate directory listing otherwise.
func (i *gatewayHandler) serveDirectory(ctx context.Context, w http.ResponseWriter, r *http.Request, resolvedPath ipath.Resolved, contentPath ipath.Path, dir files.Directory, begin time.Time, logger *zap.SugaredLogger) {
	ctx, span := tracing.Span(ctx, "Gateway", "ServeDirectory", trace.WithAttributes(attribute.String("path", resolvedPath.String())))
	defer span.End()

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
	originalURLPath := requestURI.Path

	// Ensure directory paths end with '/'
	if originalURLPath[len(originalURLPath)-1] != '/' {
		// don't redirect to trailing slash if it's go get
		// https://github.com/ipfs/kubo/pull/3963
		goget := r.URL.Query().Get("go-get") == "1"
		if !goget {
			suffix := "/"
			// preserve query parameters
			if r.URL.RawQuery != "" {
				suffix = suffix + "?" + r.URL.RawQuery
			}
			// /ipfs/cid/foo?bar must be redirected to /ipfs/cid/foo/?bar
			redirectURL := originalURLPath + suffix
			logger.Debugw("directory location moved permanently", "status", http.StatusMovedPermanently)
			http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
			return
		}
	}

	// Check if directory has index.html, if so, serveFile
	idxPath := ipath.Join(contentPath, "index.html")
	idx, err := i.api.Unixfs().Get(ctx, idxPath)
	switch err.(type) {
	case nil:
		f, ok := idx.(files.File)
		if !ok {
			internalWebError(w, files.ErrNotReader)
			return
		}

		logger.Debugw("serving index.html file", "path", idxPath)
		// write to request
		i.serveFile(ctx, w, r, resolvedPath, idxPath, f, begin)
		return
	case resolver.ErrNoLink:
		logger.Debugw("no index.html; noop", "path", idxPath)
	default:
		internalWebError(w, err)
		return
	}

	// See statusResponseWriter.WriteHeader
	// and https://github.com/ipfs/kubo/issues/7164
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

	// Generated dir index requires custom Etag (output may change between go-ipfs versions)
	dirEtag := getDirListingEtag(resolvedPath.Cid())
	w.Header().Set("Etag", dirEtag)

	if r.Method == http.MethodHead {
		logger.Debug("return as request's HTTP method is HEAD")
		return
	}

	// Optimization: use Dag.Get to fetch the children links of this directory
	// instead of UnixFS.LS. Dag.Get is faster and also provides a Size field
	// that is good enough for a directory listing.
	links, err := i.getUnixFsLinks(ctx, resolvedPath.Cid())
	if err != nil {
		internalWebError(w, err)
		return
	}

	dirListing := make([]directoryItem, 0, len(links))
	for _, link := range links {
		hash := link.Cid.String()
		di := directoryItem{
			Size:      humanize.Bytes(uint64(link.Size)),
			Name:      link.Name,
			Path:      gopath.Join(originalURLPath, link.Name),
			Hash:      hash,
			ShortHash: shortHash(hash),
		}
		dirListing = append(dirListing, di)
	}

	// construct the correct back link
	// https://github.com/ipfs/kubo/issues/1365
	backLink := originalURLPath

	// don't go further up than /ipfs/$hash/
	pathSplit := path.SplitList(contentPath.String())
	switch {
	// skip backlink when listing a content root
	case len(pathSplit) == 3: // url: /ipfs/$hash
		backLink = ""

	// skip backlink when listing a content root
	case len(pathSplit) == 4 && pathSplit[3] == "": // url: /ipfs/$hash/
		backLink = ""

	// add the correct link depending on whether the path ends with a slash
	default:
		if strings.HasSuffix(backLink, "/") {
			backLink += ".."
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
	if h, ok := r.Context().Value(requestContextKey("gw-hostname")).(string); ok {
		gwURL = "//" + h
	} else {
		gwURL = ""
	}

	dnslink := hasDNSLinkOrigin(gwURL, contentPath.String())

	// See comment above where originalUrlPath is declared.
	tplData := listingTemplateData{
		GatewayURL:  gwURL,
		DNSLink:     dnslink,
		Listing:     dirListing,
		Size:        size,
		Path:        contentPath.String(),
		Breadcrumbs: breadcrumbs(contentPath.String(), dnslink),
		BackLink:    backLink,
		Hash:        hash,
	}

	logger.Debugw("request processed", "tplDataDNSLink", dnslink, "tplDataSize", size, "tplDataBackLink", backLink, "tplDataHash", hash)

	if err := listingTemplate.Execute(w, tplData); err != nil {
		internalWebError(w, err)
		return
	}

	// Update metrics
	i.unixfsGenDirGetMetric.WithLabelValues(contentPath.Namespace()).Observe(time.Since(begin).Seconds())
}

func getDirListingEtag(dirCid cid.Cid) string {
	return `"DirIndex-` + assets.AssetHash + `_CID-` + dirCid.String() + `"`
}

func (i *gatewayHandler) getUnixFsLinks(ctx context.Context, cid cid.Cid) ([]*format.Link, error) {
	obj, err := i.api.Dag().Get(ctx, cid)
	if err != nil {
		return nil, err
	}

	protoNode, ok := obj.(*merkledag.ProtoNode)
	if !ok {
		return obj.Links(), nil
	}

	fsNode, err := unixfs.FSNodeFromBytes(protoNode.Data())
	if err != nil {
		return nil, err
	}

	if fsNode.Type() == unixfs.THAMTShard {
		shard, err := hamt.NewHamtFromDag(i.api.Dag(), obj)
		if err != nil {
			return nil, err
		}

		links, err := shard.EnumLinks(ctx)
		if err != nil {
			return nil, err
		}

		return links, nil
	} else {
		return obj.Links(), nil
	}
}
