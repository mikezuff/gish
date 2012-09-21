package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
)

// Replaces relative repo paths introduced in SVN 1.5.
// ../ -- Relative to the URL of the directory on which the svn:externals property is set
//  ^/ -- Relative to the root of the repository in which the svn:externals property is versioned
//  // -- Relative to the scheme of the URL of the directory on which the svn:externals property is set
//   / -- Relative to the root URL of the server on which the svn:externals property is versioned
func ReplaceRelative(repoRoot, externalRef string) (string, error) {
	refParts := strings.SplitAfterN(externalRef, "/", 2)

	switch refParts[0] {
	case "^/":
		return fmt.Sprint(repoRoot, "/", refParts[1]), nil
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

// Returns true if the given directory is a git repository. (Contains a .git subdir)
func IsRepo(repoPath string) bool {
	rp := path.Join(repoPath, ".git")
	info, err := os.Stat(rp)
	if err != nil {
		return false
	}

	return info.IsDir()
}

// TODO: add this file and the extern dir to .gitignore
const extCacheFilename = "git_svn_externals"

// Returns true if the given path is a dir containing only an extern cache file
func DirIsExternCacheOnly(repoPath string) bool {
	info, err := os.Stat(repoPath)
	if err != nil {
		return false // Doesn't exist
	}

	if !info.IsDir() {
		return false
	}

	dir, err := os.Open(repoPath)
	if err != nil {
		return false
	}
	defer dir.Close()

	files, err := dir.Readdirnames(4)
	if len(files) < 1 || len(files) > 3 {
		return false
	}

	for _, fn := range files {
		switch fn {
		case ".":
		case "..":
		case extCacheFilename:
			// these are acceptable
		default:
			return false
		}
	}

	return true

}

type SvnRepoInfo struct {
	Path           string
	Url            string
	RepositoryRoot string
	//RepositoryUuid string
	//Revision int
	//NodeKind string
	//Schedule string
	//LastChangedAuthor string
	//LastChangedRevision int
	//LastChangedDate string
}

func getCachedRawExternals(cachePath string) (*bytes.Buffer, error) {

	// Test for cache file
	fi, err := os.Stat(cachePath)
	if err != nil {
		return nil, err
	} else if fi.IsDir() {
		return nil, fmt.Errorf("Error %s is not an externals cache file", cachePath)
	}

	// TODO: there's a library io or ioutil that reads from file to buffer
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, fmt.Errorf("Error opening externals cache: %v", err)
	}

	// verbose? fmt.Printf("Reading cached externals from %s\n", cachePath)
	rawExtern := new(bytes.Buffer)
	_, err = rawExtern.ReadFrom(f)
	if err != nil {
		return nil, fmt.Errorf("Error reading externals cache: %v", err)
	}

	return rawExtern, nil
}

func cacheRawExternals(cachePath, rawExterns string) {
	f, err := os.Create(cachePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failure opening cache file %s", cachePath)
		return
	}

	_, err = f.WriteString(rawExterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failure writing cache file %s", cachePath)
	}
}

func getRawExternals(repoPath string) (string, error) {
	cachePath := path.Join(repoPath, extCacheFilename)

	// TODO: accept arg to force cache refresh

	rawBytes, err := getCachedRawExternals(cachePath)
	if err == nil {
		return rawBytes.String(), nil
	} else {
		// No cache found, search for alternate cache if provided.
		if alternateExternalsCache != "" {
			alternateExternalsCachePath := path.Join(alternateExternalsCache, extCacheFilename)
			rawBytes, err = getCachedRawExternals(alternateExternalsCachePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error opening alt_cache: \n", err)
			} else {
				// Alt cache is good, copy it to the current dir
				err = os.MkdirAll(path.Dir(alternateExternalsCachePath), 0660)
				if err == nil {
					err = ioutil.WriteFile(alternateExternalsCachePath, rawBytes.Bytes(), 0660)
				}

				if err == nil {
					fmt.Printf("Externals copied from alt cache to %s\n", alternateExternalsCachePath)
					return rawBytes.String(), nil
				} else {
					fmt.Fprintf(os.Stderr, "Error storing alt_cache to %s: %s", alternateExternalsCachePath, err)
				}
			}
		}
	}

	if quickMode {
		fmt.Printf("Quick mode: not reading externals for %s\n", repoPath)
		return "", nil
	}

	// No cached externals found. Get them from git-svn.
	fmt.Printf("Getting externals from server for: %s\n", repoPath)
	rawExterns, err := gitSvnShowExternals(repoPath)
	if err != nil {
		return "", err
	}

	cacheRawExternals(cachePath, rawExterns)

	return rawExterns, nil
}

// Return a SvnRepoInfo for the git-svn repo in the current directory.
func ReadGitSvnRepo(repoPath string) (SvnRepoInfo, error) {
	if !IsRepo(repoPath) {
		return SvnRepoInfo{}, fmt.Errorf("Path %s is not a git-svn repo.", repoPath)
	}

	svnrepo, err := gitSvnInfo(repoPath)
	if err != nil {
		return SvnRepoInfo{}, err
	}

	return svnrepo, nil
}

// Recursively clone or rebase externals within given git-svn repo.
func UpdateExternals(repoPath string) error {
	fmt.Printf("Rebasing %s:\n", repoPath)
    err := interactiveShellCmd(repoPath, "git", "svn", "rebase")
	if err != nil {
		return err
	}
	/* TODO: We could check for changes to the externs. If so, invalidate the cache */

	externs, err := LoadExternals(repoPath)
	if err != nil {
		return err
	}

	for _, extern := range externs {
        if IsRepo(extern.Path) {
			// TODO: if the repo exists, make sure it matches the extern url. The extern may have been relocated.
		} else if DirIsExternCacheOnly(extern.Path) {
			fmt.Printf("Cloning external %s from %s\n", extern.Path, extern.Url)

			// The directory doesn't exist, clone it.
			// TODO: Clone args should be passed in or stored in a file
			err := interactiveShellCmd(repoPath, "git", "svn", "clone", "--no-minimize-url", extern.Url, extern.Path)
			if err != nil {
				return err
			}

		} else {
			return fmt.Errorf("Directory %s exists but is not git repo.", extern.Path)
		}

		// The extern is ready, check it.
		err = UpdateExternals(extern.Path)
		if err != nil {
			return err
		}

	}

	return nil
}

func ChanLoad(repoPath string, starts chan int, infoChan chan SvnRepoInfo) {
	if !IsRepo(repoPath) {
		fmt.Printf("IGNORED: Path %s is not a git-svn repo.\n", repoPath)
		starts <- -1 // We're not going to put an infoChan for this repo
		return
	}

	// Get svn info for the current directory
	repoInfo, err := gitSvnInfo(repoPath)
	if err != nil {
        log.Fatal("Error getting svn info:", err)
	}

	externs, err := LoadExternals(repoPath)
	if err != nil {
        log.Fatal("Error loading externals:", err)
	}

	// Send the number of repo searches that will be started
	starts <- len(externs)

	for _, extern := range externs {
		go ChanLoad(extern.Path, starts, infoChan)
	}

	infoChan <- repoInfo
}

// Load the svn info for the git-svn repo in the given directory and all known
// externals. 
func ReadFullGitSvnRepo(repoPath string) (repoList []SvnRepoInfo, err error) {

	infoChan := make(chan SvnRepoInfo)
	starts := make(chan int)
	go ChanLoad(repoPath, starts, infoChan)

	rx := 1 + <-starts

	for rx > 0 {
		select {
		case n := <-starts:
			rx += n
		case info := <-infoChan:
			repoList = append(repoList, info)
			rx--
		}
	}

	return
}

func LoadExternals(repoPath string) (externs []SvnRepoInfo, err error) {
	repo, err := ReadGitSvnRepo(repoPath)
	if err != nil {
		err = fmt.Errorf("Error %v, loading %v", err, repoPath)
		return
	}

	rawExterns, err := getRawExternals(repoPath)
	if err == nil {
		externs = cookExternals(repo, rawExterns)
	}

	return
}

func cookExternals(parent SvnRepoInfo, rawExternals string) (cooked []SvnRepoInfo) {
	i := 0

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
				//fmt.Printf("PATH matched %s\n", lastPath[1])
			} else {
				//fmt.Printf("PATH Ignoring line %s\n", strings.TrimSpace(line))
			}
		} else if expecting == EXT {
			pat := fmt.Sprintf(`^%s(\S*)\s(.*)`, regexp.QuoteMeta(lastPath[1]))
			extRegex := regexp.MustCompile(pat)
			match := extRegex.FindStringSubmatch(line)
			if match == nil {
				//fmt.Printf("EXT Ignoring line %s\n", strings.TrimSpace(line))
			} else {
				svnUrl, err := ReplaceRelative(parent.RepositoryRoot, match[1])
				if err != nil {
					fmt.Errorf("Error with extern %v\n", err)
				} else {
					extPath := path.Join(parent.Path, lastPath[1], match[2])
					cooked = append(cooked, SvnRepoInfo{Path: extPath, Url: svnUrl})
					//fmt.Printf("New external: %s => %s\n", extPath, svnUrl)
				}
			}
			expecting = PATH
		}
		i += 1
	}

	return cooked
}

func gitSvnShowExternals(repoPath string) (string, error) {
	return interactiveShellCmdToString(repoPath, "git", "svn", "show-externals")
}

func gitSvnInfo(repoPath string) (info SvnRepoInfo, err error) {
	out, err := shellCmd(repoPath, "git", "svn", "info")
	if err != nil {
		return
	}

	attrs := strings.SplitAfter(out, "\n")

	// The path given by svn info is relative to the current directory
	// so it's always '.' :(
	//info.Path = getValOrPanic("Path", attrs[0])
	info.Path = repoPath
	info.Url = getValOrPanic("URL", attrs[1])
	info.RepositoryRoot = getValOrPanic("Repository Root", attrs[2])

	return
}

// Split the src string by key:val, trimming whitespace off the value.
// Return the value if the key == expectedKey, panic otherwise.
func getValOrPanic(expectedKey string, src string) string {
	l := strings.SplitN(src, ":", 2)

	if l[0] != expectedKey {
		panic(fmt.Sprintf("Key %s doesn't match expected key %s", l[0], expectedKey))
	}

	return strings.TrimSpace(l[1])
}

func MakeGitCommand(args []string) func(repo SvnRepoInfo) {
	return func(repo SvnRepoInfo) {
		interactiveShellCmd(repo.Path, "git", args...)
	}
}

// Call the action function for each repo found in rootPath
func ForeachRepo(rootPath string, action func(SvnRepoInfo)) error {
	repoList, err := ReadFullGitSvnRepo(rootPath)
	if err != nil {
		return err
	}

	if len(repoList) == 0 {
		return fmt.Errorf("Path %s is not a git repo.\n", rootPath)
	}

	for _, repo := range repoList {
		fmt.Printf("Repo %s:\n", repo.Path)
		action(repo)
	}

	return nil
}

func usage() {
	cmdName := "gish" // Not: os.Args[0]
	fmt.Fprintf(os.Stderr, "%s - recursively perform commands on a git-svn repo and its externals\n",
		cmdName)
	fmt.Fprintf(os.Stderr, "Usage:\n\t%s [options] <command>\n", cmdName)
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "\tlist: print all the known git-svn repos in this directory\n")
	fmt.Fprintf(os.Stderr, "\tupdate: clone/rebase all the externals contained by the git-svn repo\n")
	fmt.Fprintf(os.Stderr, "\n\tOther commands are passed directly to git along with their arguments.\n")

	fmt.Fprintf(os.Stderr, "Options:\n")
	flag.PrintDefaults()
}

var quickMode bool
var alternateExternalsCache string

func main() {
	flag.BoolVar(&quickMode, "q", false, "Quick mode.")
	flag.StringVar(&alternateExternalsCache, "alt_ext_cache", "",
		"Alternate path to search for externals cache file.")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		log.Fatal("No command provided.")
	}

	pwd, err := os.Getwd()
	if err != nil {
		log.Fatal("Error getting pwd: ", err)
	}

	switch args[0] {
	case "update":
		err = UpdateExternals(pwd)
	case "list":
		repoList, err := ReadFullGitSvnRepo(pwd)
		if err == nil {
			fmt.Printf("Found %d repos:\n", len(repoList))
			for _, repo := range repoList {
				fmt.Println(repo.Path)
			}
		}
	default:
		// Simple recursive commands don't need any help.
		err = ForeachRepo(pwd, MakeGitCommand(args))
	}

	if err != nil {
		log.Fatal("Error: ", err)
	}
}
