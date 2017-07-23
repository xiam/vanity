package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/namsral/flag"
)

var (
	addrFlag       = flag.String("addr", ":8080", "Serve HTTP at given address")
	socketFlag     = flag.String("socket", "", "Serve HTTP at given UNIX socket")
	vanityRootFlag = flag.String("vanity_root", "", "Vanity root URL (e.g.: https://upper.io).")
	repoRootFlag   = flag.String("repo_root", "", "Git repository root URL (e.g.: https://github.com/upper).")
)

var packagePattern = regexp.MustCompile(`^/([-a-zA-Z0-9]+)\.?(v([0-9]*))?(.*)$`)

var httpClient = &http.Client{Timeout: 10 * time.Second}

const refsSuffix = ".git/info/refs?service=git-upload-pack"

// Error messages.
var (
	ErrNoRepo    = errors.New("repository not found")
	ErrNoVersion = errors.New("version reference not found")
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("Could not start server: %q", err)
	}
}

func run() error {
	flag.Parse()

	if *addrFlag == "" && *socketFlag == "" {
		return fmt.Errorf("must provide -addr")
	}

	if *repoRootFlag == "" {
		return fmt.Errorf("must provide -repo-root")
	}

	if *vanityRootFlag == "" {
		return fmt.Errorf("must provide -vanity-root")
	}

	repoRoot, err := NewRepoRoot(*repoRootFlag, *vanityRootFlag)
	if err != nil {
		return fmt.Errorf("could not parse -repo-root: %q", err)
	}

	var listenAddr, listenNet string

	if *socketFlag != "" {
		listenNet, listenAddr = "unix", *socketFlag
	} else {
		listenNet, listenAddr = "tcp", *addrFlag
	}

	li, err := net.Listen(listenNet, listenAddr)
	if err != nil {
		return fmt.Errorf("Failed to bind to %s %s: %v", listenNet, listenAddr, err)
	}

	http.HandleFunc("/", newHandler(repoRoot))

	log.Printf("Listening at %s. %s -> %s", listenAddr, *vanityRootFlag, *repoRootFlag)

	return http.Serve(li, nil)
}

var gogetTemplate = template.Must(template.New("").Parse(`
<html>
<head>
<meta name="go-import" content="{{.VanityPath}} git {{.VanityURL}}">
<meta name="go-source" content="{{.VanityPath}} _ {{.RepoRootURL}}/tree/{{.GitTree}}{/dir} {{.RepoRootURL}}/blob/{{.GitTree}}{/dir}/{file}#L{line}">
</head>
<body>
go get {{.VanityPath}}
</body>
</html>
`))

// RepoRoot represents a real repository and vanity name pair.
type RepoRoot struct {
	vanityURL      *url.URL
	repoURL        *url.URL
	RepoHostPath   string
	VanityHostPath string
}

func parseRepoURL(in string) (*url.URL, error) {
	u, err := url.Parse(in)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u, err
}

// NewRepoRoot creates a RepoRoot
func NewRepoRoot(repoURL string, vanityURL string) (*RepoRoot, error) {
	u, err := parseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}

	v, err := parseRepoURL(vanityURL)
	if err != nil {
		return nil, err
	}

	return &RepoRoot{
		repoURL:        u,
		vanityURL:      v,
		RepoHostPath:   u.Host + u.Path,
		VanityHostPath: v.Host + v.Path,
	}, nil
}

// NewRepo creates a new repository.
func (root *RepoRoot) NewRepo(name string) *Repo {
	return &Repo{
		Root: root,
		Name: name,
	}
}

// Repo represents a source code repository on GitHub.
type Repo struct {
	Root *RepoRoot

	Name  string
	Major string

	RequestedVersion semver.Version

	// FullVersion is the best version in AllVersions that matches MajorVersion.
	// It defaults to InvalidVersion if there are no matches.
	FullVersion *semver.Version

	// AllVersions holds all versions currently available in the repository,
	// either coming from branch names or from tag names. Version zero (v0)
	// is only present in the list if it really exists in the repository.
	AllVersions semver.Versions
}

// SetVersions records in the relevant fields the details about which
// package versions are available in the repository.
func (repo *Repo) SetVersions(all semver.Versions) {
	repo.AllVersions = all
	for _, v := range repo.AllVersions {
		if v.Major == repo.RequestedVersion.Major && (repo.FullVersion == nil || repo.FullVersion.LessThan(*v)) {
			repo.FullVersion = v
		}
	}
}

// RepoRoot returns the repository root, without a schema.
func (repo *Repo) RepoRoot() string {
	return repo.Root.RepoHostPath + "/go-" + repo.Name
}

// VanityRoot returns the vanity repository root, without a schema.
func (repo *Repo) VanityRoot() string {
	return repo.Root.VanityHostPath + "/" + repo.Name
}

// GitTree returns the repository tree name for the selected version.
func (repo *Repo) GitTree() string {
	if repo.FullVersion == nil || repo.Major == "" {
		return "master"
	}
	return repo.FullVersion.String()
}

// VanityPath returns the real package path, without a schema.
func (repo *Repo) VanityPath() string {
	if repo.Major == "" {
		return repo.VanityRoot()
	}
	return repo.VanityRoot() + ".v" + repo.Major
}

// VanityURL returns the vanity package's URL.
func (repo *Repo) VanityURL() string {
	return repo.Root.vanityURL.Scheme + "://" + repo.VanityPath()
}

// RepoRootURL returns the real package's URL.
func (repo *Repo) RepoRootURL() string {
	return repo.Root.repoURL.Scheme + "://" + repo.RepoRoot()
}

func newHandler(repoRoot *RepoRoot) func(http.ResponseWriter, *http.Request) {
	return func(resp http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health-check" {
			resp.Write([]byte("ok"))
			return
		}
		log.Printf("%s requested %s", req.RemoteAddr, req.URL)

		if req.URL.Path == "/" {
			sendNotFound(resp, "Missing package name.")
			return
		}

		u, err := url.Parse(req.URL.Path)
		if err != nil {
			sendError(resp, "Failed to parse request path")
			return
		}

		p := packagePattern.FindStringSubmatch(u.Path)

		pkgName, _, version, extra := p[1], p[2], p[3], p[4]
		repo := repoRoot.NewRepo(pkgName)

		var requestedVersion semver.Version
		if version != "" {
			repo.Major = version
			repo.RequestedVersion.Major, _ = strconv.ParseInt(repo.Major, 10, 64)
		}

		var changed []byte
		var versions semver.Versions
		original, err := fetchRefs(repo)
		if err == nil {
			changed, versions, err = changeRefs(original, &repo.RequestedVersion)
			repo.SetVersions(versions)
		}

		switch err {
		case nil:
			// all ok
		case ErrNoRepo:
			sendNotFound(resp, "Git repository not found at https://%s", repo.RepoRoot())
			return
		case ErrNoVersion:
			sendNotFound(resp, `Git repository at https://%s has no tag %v`, repo.RepoRoot(), requestedVersion)
			return
		default:
			resp.WriteHeader(http.StatusBadGateway)
			resp.Write([]byte(fmt.Sprintf("Cannot obtain refs from Git: %v", err)))
			return
		}

		switch extra {
		case `/git-upload-pack`:
			proxyURL := "https://" + repo.RepoRoot() + "/git-upload-pack"

			proxyReq, err := http.NewRequest(req.Method, proxyURL, req.Body)
			if err != nil {
				resp.WriteHeader(http.StatusInternalServerError)
				return
			}
			for k, v := range req.Header {
				proxyReq.Header[k] = v
			}

			proxyRes, err := http.DefaultClient.Do(proxyReq)
			if err != nil {
				log.Printf("Proxy: %v", err)
				resp.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer proxyRes.Body.Close()

			for k, v := range proxyRes.Header {
				resp.Header()[k] = v
			}

			buf, err := ioutil.ReadAll(proxyRes.Body)
			if err != nil {
				log.Printf("Proxy: %v", err)
				resp.WriteHeader(http.StatusBadGateway)
				return
			}

			if _, err = resp.Write(buf); err != nil {
				log.Printf("Proxy: %v", err)
				resp.WriteHeader(http.StatusInternalServerError)
				return
			}
			return
		case `/info/refs`:
			resp.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			resp.Write(changed)
			return
		}

		resp.Header().Set("Content-Type", "text/html")
		if req.FormValue("go-get") == "1" {
			// execute simple template when this is a go-get request
			err = gogetTemplate.Execute(resp, repo)
			if err != nil {
				log.Printf("error executing go get template: %s\n", err)
			}
			return
		}

		sendNotFound(resp, "Missing ?go-get=1 parameter.")
	}
}

func sendError(resp http.ResponseWriter, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	resp.WriteHeader(http.StatusInternalServerError)
	resp.Write([]byte(msg))
}

func sendNotFound(resp http.ResponseWriter, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	resp.WriteHeader(http.StatusNotFound)
	resp.Write([]byte(msg))
}

func fetchRefs(repo *Repo) (data []byte, err error) {
	repoURL := repo.RepoRootURL() + refsSuffix
	resp, err := httpClient.Get(repoURL)
	if err != nil {
		return nil, fmt.Errorf("cannot talk to git repository: %v", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// ok
	case 401, 404:
		return nil, ErrNoRepo
	default:
		return nil, fmt.Errorf("error from git repository: %v", resp.Status)
	}

	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading from git: %v", err)
	}
	return data, err
}

func changeRefs(data []byte, major *semver.Version) (changed []byte, versions semver.Versions, err error) {
	var hlinei, hlinej int // HEAD reference line start/end
	var mlinei, mlinej int // master reference line start/end
	var vrefhash string
	var vrefname string
	var vrefv *semver.Version

	// Record all available versions, the locations of the master and HEAD lines,
	// and details of the best reference satisfying the requested major version.
	versions = semver.Versions{}
	sdata := string(data)
	for i, j := 0, 0; i < len(data); i = j {
		size, err := strconv.ParseInt(sdata[i:i+4], 16, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot parse refs line size: %s", string(data[i:i+4]))
		}
		if size == 0 {
			size = 4
		}
		j = i + int(size)
		if j > len(sdata) {
			return nil, nil, fmt.Errorf("incomplete refs data received from GitHub")
		}
		if sdata[0] == '#' {
			continue
		}

		hashi := i + 4
		hashj := strings.IndexByte(sdata[hashi:j], ' ')
		if hashj < 0 || hashj != 40 {
			continue
		}
		hashj += hashi

		namei := hashj + 1
		namej := strings.IndexAny(sdata[namei:j], "\n\x00")
		if namej < 0 {
			namej = j
		} else {
			namej += namei
		}

		name := sdata[namei:namej]

		if name == "HEAD" {
			hlinei = i
			hlinej = j
		}
		if name == "refs/heads/master" {
			mlinei = i
			mlinej = j
		}

		if strings.HasPrefix(name, "refs/tags/v") {
			if !strings.HasSuffix(name, "^{}") {
				continue // Only accept annotated tags.
			}
			// Annotated tag is peeled off and overrides the same version just parsed.
			name = name[:len(name)-3]

			v, err := semver.NewVersion(name[strings.IndexByte(name, 'v')+1:])
			if err == nil {
				versions = append(versions, v)
				if major.Major == v.Major && (vrefv == nil || v == vrefv || vrefv.LessThan(*v)) {
					vrefv = v
					vrefhash = sdata[hashi:hashj]
					vrefname = name
				}
			}
		}
	}

	// If the file has no HEAD line or the version was not found, report as unavailable.
	if hlinei == 0 || vrefhash == "" {
		return nil, nil, ErrNoVersion
	}

	var buf bytes.Buffer
	buf.Grow(len(data) + 256)

	// Copy the header as-is.
	buf.Write(data[:hlinei])

	// Extract the original capabilities.
	caps := ""
	if i := strings.Index(sdata[hlinei:hlinej], "\x00"); i > 0 {
		caps = strings.Replace(sdata[hlinei+i+1:hlinej-1], "symref=", "oldref=", -1)
	}

	// Insert the HEAD reference line with the right hash and a proper symref capability.
	var line string
	if strings.HasPrefix(vrefname, "refs/heads/") {
		if caps == "" {
			line = fmt.Sprintf("%s HEAD\x00symref=HEAD:%s\n", vrefhash, vrefname)
		} else {
			line = fmt.Sprintf("%s HEAD\x00symref=HEAD:%s %s\n", vrefhash, vrefname, caps)
		}
	} else {
		if caps == "" {
			line = fmt.Sprintf("%s HEAD\n", vrefhash)
		} else {
			line = fmt.Sprintf("%s HEAD\x00%s\n", vrefhash, caps)
		}
	}
	fmt.Fprintf(&buf, "%04x%s", 4+len(line), line)

	// Insert the master reference line.
	line = fmt.Sprintf("%s refs/heads/master\n", vrefhash)
	fmt.Fprintf(&buf, "%04x%s", 4+len(line), line)

	// Append the rest, dropping the original master line if necessary.
	if mlinei > 0 {
		buf.Write(data[hlinej:mlinei])
		buf.Write(data[mlinej:])
	} else {
		buf.Write(data[hlinej:])
	}

	return buf.Bytes(), versions, nil
}
