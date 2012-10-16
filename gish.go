package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultCheckoutArgs = "--no-minimize-url"

	cacheRelPath = ".git/info/gish.conf"
	oldCachePath = "git_svn_externals"
)

var (
	altConfig = flag.String("c", "", "Path to config file to use if no other is found.")
)

func UsageExit(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	Usage()
	os.Exit(1)
}

func Usage() {
	fmt.Fprint(os.Stderr, "gish - recursively perform commands on a git-svn repo and its externals\n")
	fmt.Fprint(os.Stderr, "Usage:\n\tgish [options] <command>\n")
	fmt.Fprint(os.Stderr, "Commands:\n")
	fmt.Fprint(os.Stderr, "\tclone: clone the repo's externals.\n")
	fmt.Fprint(os.Stderr, "\tlist: print all the known git-svn repos in this directory\n")
	fmt.Fprint(os.Stderr, "\n\tOther commands are passed directly to git along with their arguments.\n")

	fmt.Fprint(os.Stderr, "Options:\n")
	flag.PrintDefaults()
}

// Returns true if the given directory is a git repository. (Contains a .git subdir)
func IsRepo(repoPath string) bool {
	rp := path.Join(repoPath, ".git")
	info, err := os.Stat(rp)
	if err != nil {
		return false
	}

	return info.IsDir()
}

func IsDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Return the path to the outermost repo containing the current path.
func FindRootRepoPath() (string, error) {
	pwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error getting pwd: ", err)
		os.Exit(1)
	}

	// XXX: non-portable
	parts := strings.SplitAfter(pwd, "/")
	for i, _ := range parts {
		testPath := path.Join(parts[:i+1]...)
		if IsRepo(testPath) {
			return testPath, nil
		}
	}

	// Return pwd in case we're cloning into pwd.
	return pwd, fmt.Errorf("No .git found in %s or any parent dir.", pwd)
}

// Get svn info for the repo. Label is the string to the left of the colon in the 
// standard svn info format. RepoPath must be a git-svn repo.
func GitSvnInfo(repoPath, label string) (string, error) {
	out, err := shellCmd(repoPath, "git", "svn", "info")
	if err != nil {
		return "", fmt.Errorf("git svn info failed (%s), not a git repo??\n", err)
	}

	lines := strings.SplitAfter(out, "\n")
	for _, line := range lines {
		w := strings.SplitN(line, ":", 2)
		if w[0] == label {
			return strings.TrimSpace(w[1]), nil
		}
	}
	return "", fmt.Errorf("attribute %s not found in git svn info", label)
}

// Replaces relative repo paths introduced in SVN 1.5.
// ../ -- Relative to the URL of the directory on which the svn:externals property is set
//  ^/ -- Relative to the root of the repository in which the svn:externals property is versioned
//  // -- Relative to the scheme of the URL of the directory on which the svn:externals property is set
//   / -- Relative to the root URL of the server on which the svn:externals property is versioned
func ReplaceRelative(repoRootUrl, externalRef string) (string, error) {
	refParts := strings.SplitAfterN(externalRef, "/", 2)

	switch refParts[0] {
	case "^/":
		return fmt.Sprint(repoRootUrl, "/", refParts[1]), nil
	case "../":
		fallthrough
	case "//":
		fallthrough
	case "/":
		return "", errors.New("Unhandled relative extern type")
	}

	// No relative content
	return externalRef, nil
}

func GitSvnUrl(repoPath string) (url string, err error) {
	out, err := shellCmd(repoPath, "git", "svn", "info")
	if err != nil {
		return "", err
	}

	lines := strings.SplitAfter(out, "\n")
	for _, line := range lines {
		w := strings.SplitN(line, ":", 2)
		if w[0] == "URL" {
			return w[1], nil
		}
	}
	return "", fmt.Errorf("Attribute URL not found in git svn info for %s", repoPath)
}

type Repo struct {
	Path           string
	Url            string
	CheckoutArgs   string
	ExternalsKnown bool
	Externals      []Repo
	Root           *Repo `json:"-"` // Don't include in json
}

func (repo *Repo) LoadExternals() error {
	rawExternals, err := interactiveShellCmdToString(repo.Path, "git", "svn", "show-externals")
	if err != nil {
		return err
	}

	return repo.CookExternals(rawExternals)
}

func (repo *Repo) CookExternals(rawExternals string) error {

	const (
		PATH = iota
		EXT
	)

	var lastPath []string
	pathRegex := regexp.MustCompile(`^#\s(.*)`)
	lines := strings.SplitAfter(rawExternals, "\n")
	expecting := PATH
	for _, line := range lines {
		if expecting == PATH {
			lastPath = pathRegex.FindStringSubmatch(line)
			if lastPath != nil {
				expecting = EXT
			} else {
			}
		} else if expecting == EXT {
			pat := fmt.Sprintf(`^%s(\S*)\s(.*)`, regexp.QuoteMeta(lastPath[1]))
			extRegex := regexp.MustCompile(pat)
			match := extRegex.FindStringSubmatch(line)
			if match != nil {
				repoRoot, err := GitSvnInfo(repo.Path, "Repository Root")
				if err != nil {
					return err
				}

				svnUrl, err := ReplaceRelative(repoRoot, match[1])
				if err != nil {
					return fmt.Errorf("Error with extern %v\n", err)
				} else {
					extPath := path.Join(repo.Path, lastPath[1], match[2])
					repo.Externals = append(repo.Externals,
						Repo{Path: extPath, Url: svnUrl, Root: repo.Root})
				}
			}
			expecting = PATH
		}
	}

	repo.ExternalsKnown = true
	return nil
}

func (repo *Repo) List() {
	fmt.Println(repo.Path)
	for _, ext := range repo.Externals {
		ext.List()
	}
}

// Return a slice of the paths of the repo and all its externs
func (repo *Repo) Paths() []string {
	p := []string{repo.Path}
	for _, ext := range repo.Externals {
		p = append(p, ext.Paths()...)
	}

	return p
}

// Link externals to a root repo
func LinkTo(externs []Repo, root *Repo) {
	for i := range externs {
		externs[i].Root = root
		LinkTo(externs[i].Externals, root)
	}
}

// Link Root of all child repos to this repo
func (repo *Repo) LinkRoot() {
	LinkTo(repo.Externals, repo)
}

func RewritePaths(repo *Repo, a, b string) {
	repo.Path = strings.Replace(repo.Path, a, b, 1)
	for i := range repo.Externals {
		RewritePaths(&repo.Externals[i], a, b)
	}
}

// Check that the repo and its externals are cloned.
func (repo *Repo) Clone() error {
	repoPath, repoDir := path.Split(repo.Path)

	if IsRepo(repo.Path) {
		fmt.Printf("Path %s is a repo, updating from svn.\n", repo.Path)
		err := interactiveShellCmd(repo.Path, "git", "svn", "rebase")
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("Cloning %q from svn url %q\n", repo.Path, repo.Url)

		err := os.MkdirAll(repo.Path, 0770)
		if err != nil {
			return err
		}

		args := []string{"svn", "clone"}
		if repo.CheckoutArgs != "" {
			args = append(args, strings.Split(repo.CheckoutArgs, " ")...)
		} else {
			args = append(args, defaultCheckoutArgs)
		}
		args = append(args, repo.Url, repoDir)
		fmt.Printf("> git %v\n", args)
		err = interactiveShellCmd(repoPath, "git", args...)
		if err != nil {
			return err
		}
	}

	if !repo.ExternalsKnown {
		err := repo.LoadExternals()
		if err != nil {
			return err
		}
	}

	// Save the externals
	repo.WriteConfig()

	for i := range repo.Externals {
		err := repo.Externals[i].Clone()
		if err != nil {
			return err
		}
	}

	return nil
}

// Do a 'git clean' on each repo, removing the externals from the list.
func (repo *Repo) Clean() error {
	toRmStr, err := shellCmd(repo.Path, "git", "clean", "-ndx")
	if err != nil {
		return err
	}

	// Build a map of the externs
	extMap := make(map[string]bool, len(repo.Externals))
	for _, ext := range repo.Externals {
		extRelPath := strings.Trim(strings.Replace(ext.Path, repo.Path, "", 1), "/")
		extMap[extRelPath] = true
	}

	toRm := strings.Split(toRmStr, "\n")
	for i := range toRm {
		r := strings.Replace(toRm[i], "Would remove ", "", 1)
		r = strings.Trim(r, "/")

		if r == "" {
			continue
		}

		qualifiedR := path.Join(repo.Path, r)

		if !extMap[r] {
			enable := true // TODO: flag
			if enable {
				err = os.RemoveAll(qualifiedR)
				if err != nil {
					fmt.Fprintln(os.Stdout, err)
				}
			} else {
				fmt.Printf("Would remove %q\n", qualifiedR)
			}
		} else {
			fmt.Printf("Ignoring external at %q\n", qualifiedR)
		}
	}

	for _, ext := range repo.Externals {
		err = ext.Clean()
		if err != nil {
			return err
		}
	}

	return nil
}

// Load the old-style externals cache into the repo.
// repo.Path should be initialized beforehand.
func (repo *Repo) ConvertExternCache() error {
	fullCachePath := path.Join(repo.Path, oldCachePath)
	b, err := ioutil.ReadFile(fullCachePath)
	if err != nil {
		return err
	}

	repo.Url, err = GitSvnInfo(repo.Path, "URL")
	if err != nil {
		return err
	}

	buf := bytes.NewBuffer(b)
	err = repo.CookExternals(buf.String())
	if err != nil {
		return err
	} else {
		// TODO: why is extern a copy in
		// for  _, extern := range repo.externals
		for i := range repo.Externals {
			err = repo.Externals[i].ConvertExternCache()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error converting old cache: ", err)
			}
		}
	}

	err = os.Remove(fullCachePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error deleting old cache: ", err)
	}

	return nil
}

// If necessary, write the repo configuration to file.
func (repo *Repo) WriteConfig() error {
	if repo.Root != nil {
		return repo.Root.WriteConfig()
	} else {
		b, err := json.MarshalIndent(repo, "", "  ")
		if err != nil {
			return err
		}

		return ioutil.WriteFile(path.Join(repo.Path, cacheRelPath), b, 0660)
	}
	panic("Unreachable")
}

// Create a Repo from a config file at the given location.
// Location can be a path to a git repo or to a config file.
func LoadConfig(repoPath string) (repo *Repo, err error) {
	isDir := IsDir(repoPath)
	cachePath := repoPath
	if isDir {
		cachePath = path.Join(repoPath, cacheRelPath)
	}

	// Look for new config
	b, err := ioutil.ReadFile(cachePath)
	if err == nil {
		repo = new(Repo)
		err = json.Unmarshal(b, repo)
	} else {
		// Look for old externals cache
		if isDir {
			cachePath = path.Join(repoPath, oldCachePath)
		}
		_, err = os.Stat(cachePath)
		if err == nil {
			repo := &Repo{Path: repoPath}
			err = repo.ConvertExternCache()
		} else {
			err = fmt.Errorf("No config found in %s", repoPath)
		}
	}

	if repo != nil {
		repo.LinkRoot()
	}

	return repo, err
}

// TODO: use cases handled?
// gish use cases:
// * completely new, no config
//       gish clone svn://svnserver/repo [destdir]
// * alt config
//    1) Completely new
//       gish clone --config=alt [destdir]
//    2) Existing repo (any command).
//       gish --config=alt [command]
//       -- Check urls against repo
//       -- Assume externs are right??
// * Clone of root, no config
// TODO: Clone of existing git-svn repo?
func NewRepo(cmdLineArgs []string) (*Repo, error) {
	rootPath, err := FindRootRepoPath()
	if err != nil {
		if cmdLineArgs[0] == "clone" {
			// Clone can be used three ways, two are handled here
			if *altConfig != "" {
				// Usage is:  gish clone --config=alt destdir
				if len(cmdLineArgs) != 2 {
					UsageExit("Invalid args to 'gish clone'.")
				}

				destDir, err := filepath.Abs(cmdLineArgs[1])
				if err != nil {
					UsageExit(fmt.Sprintf("invalid destdir %s: %v", cmdLineArgs[1], err))
				}

				repo, err := LoadConfig(*altConfig)
				if err != nil {
					fmt.Fprintln(os.Stderr,
						"Provided alternate config is invalid: ", err.Error())
					os.Exit(1)
				}

				RewritePaths(repo, repo.Path, destDir)
				return repo, nil
			} else {
				// TODO: is this usage documented?
				// TODO: do both w/ and w/o destdir work?
				// Usage is:  gish clone svn://svnserver/repo [destdir]

				if len(cmdLineArgs) < 2 {
					UsageExit("Not enough arguments to 'gish clone'.")
				}

				// Fill in the url provided, clone will fill the rest
				svnUrl, err := url.Parse(strings.TrimSpace(cmdLineArgs[1]))
				if err != nil {
					UsageExit(fmt.Sprint("Error parsing svn Url: %q", err.Error()))
				}

				var destDir string
				if len(cmdLineArgs) == 3 {
					destDir = cmdLineArgs[2]
				} else {
					pathParts := strings.Split(svnUrl.Path, "/")
					destDir = pathParts[len(pathParts)-1]
				}

				absDestDir, err := filepath.Abs(destDir)
				if err != nil {
					UsageExit(fmt.Sprintf("invalid destdir %s: %v", destDir, err))
				}

				return &Repo{Path: absDestDir, Url: svnUrl.String()}, nil
			}
		} else {
			return nil, err
		}
	}

	repo, err := LoadConfig(rootPath)
	if err != nil {
		fmt.Println(err)
		fmt.Printf("Loading info from git. This may take a while.\n")
		url, err := GitSvnInfo(rootPath, "URL")
		if err != nil {
			return nil, err
		}

		repo := &Repo{Path: rootPath, Url: url}

		err = repo.LoadExternals()
		if err != nil {
			return nil, err
		}
	}

	return repo, nil
}

func main() {
	flag.Parse()

	cmdLineArgs := flag.Args()
	if len(cmdLineArgs) == 0 {
		UsageExit("No command provided.")
	}

	repo, err := NewRepo(cmdLineArgs)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// TODO: add externs to .git/info/exclude

	switch cmdLineArgs[0] {
	case "clone":
		err = repo.Clone()
		if err != nil { // Skip the config write. Clone() writes config for each successful clone.
			fmt.Println(err)
			os.Exit(1)
		}
	case "list":
		repo.List()
	case "clean":
		repo.Clean()
	default:
		paths := repo.Paths()
		for _, path := range paths {
			fmt.Printf("Repo %s:\n", path)
			err = interactiveShellCmd(path, "git", cmdLineArgs...)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Git returned error:", err)
				// Don't quit, commands that get paged will return error.
			}
		}
	}

	err = repo.WriteConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error writing config: ", err)
	}
}
