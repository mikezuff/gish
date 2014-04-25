package main

// gish - recursively perform commands on a git-svn repo and its externals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	pathLib "path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultCheckoutArgs = "--no-minimize-url"

	ignoreRelPath       = ".git/info/exclude"
	gishCachePathV2     = ".git/info/gish.conf"
	gishCachePathV1     = "git_svn_externals"
	gishNotesRef        = "GIT_NOTES_REF=refs/notes/gish"
	persistWithGitNotes = true
)

var (
	dryRun, force bool // cmdClean
)

func UsageExit(usage func(), msg string) {
	fmt.Fprintln(os.Stderr, msg)
	usage()
	os.Exit(1)
}

func Usage() {
	fmt.Fprint(os.Stderr, "usage:\n\tgish <command> [options]\n")
	fmt.Fprint(os.Stderr, "Commands:\n")
	fmt.Fprint(os.Stderr, "\tclone: clone the repo's externals.\n")
	fmt.Fprint(os.Stderr, "\tlist: list the root path of the current git repo and the paths to its externals.\n")
	fmt.Fprint(os.Stderr, "\tclean: perform git clean without removing externals\n")
	fmt.Fprint(os.Stderr, "\tupdateignores: add externals to git ignore. Done automatically with clone.\n")
	fmt.Fprint(os.Stderr, "\n\tOther commands are passed directly to git along with their arguments.\n")
	fmt.Fprint(os.Stderr, "\n\tUse 'gish <command> -h' for command-specific help.\n")

	/*
		fmt.Fprint(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	*/
}

// Execute the given command with its input connected to stdin.
func execCmdAttached(dir, arg0 string, args ...string) error {
	cmd := exec.Command(arg0, args...)
	cmd.Env = os.Environ()
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Execute the given command connecting its input to stdin, return its output as a byte slice.
func execCmd(dir, arg0 string, args ...string) ([]byte, error) {
	cmd := exec.Command(arg0, args...)
	cmd.Env = os.Environ()
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	return cmd.CombinedOutput()
}

func execCmdEnv(dir string, env []string, arg0 string, args ...string) ([]byte, error) {
	cmd := exec.Command(arg0, args...)
	if env == nil {
		cmd.Env = os.Environ()
	} else {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	return cmd.CombinedOutput()
}

func execGishNotes(path string, args ...string) ([]byte, error) {
	return execCmdEnv(path, []string{gishNotesRef}, "git", append([]string{"notes"}, args...)...)
}

// GitCreateObject creates a hashed object containing the given blob.
// Returns a string containing the object hash or git error message if error != nil.
func GitCreateObject(path string, blob []byte) (string, error) {
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Env = os.Environ()
	cmd.Dir = path
	cmd.Stdin = bytes.NewBuffer(blob)
	out, err := cmd.CombinedOutput()
	outStr := string(bytes.TrimSpace(out))
	fmt.Println("hash-object OUT:", outStr)
	return outStr, err
}

func GitNoteAdd(path string, note []byte) error {
	hash, err := GitCreateObject(path, note)
	if err != nil {
		return err
	}

	out, err := execGishNotes(path, "add", "-f", "-C", hash)
	fmt.Println("notesadd OUT:", out)
	return err
}

func GitLookupLatestGishNote(path string) (string, error) {
	out, err := execGishNotes(path, "list")
	if err != nil {
		return "", err
	}

	// Get the hash of the object that the note references.
	b := bytes.NewBuffer(out)
	_, err = b.ReadBytes(' ') // Ignore note hash
	if err != nil {
		return "", err
	}

	notedObjHash, err := b.ReadBytes('\n')
	if err != nil {
		return "", err
	}

	return string(bytes.TrimSpace(notedObjHash)), nil
}

// Returns true if the given directory is a git repository. (Contains a .git subdir)
func IsRepo(repoPath string) bool {
	rp := pathLib.Join(repoPath, ".git")
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

	parts := strings.SplitAfter(pwd, string(os.PathSeparator))
	for i, _ := range parts {
		testPath := pathLib.Join(parts[:i+1]...)
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
	out, err := execCmd(repoPath, "git", "svn", "info")
	if err != nil {
		return "", fmt.Errorf("git svn info failed (%s), not a git repo??\n", err)
	}

	lines := strings.SplitAfter(string(out), "\n")
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
	out, err := execCmd(repoPath, "git", "svn", "info")
	if err != nil {
		return "", err
	}

	lines := strings.SplitAfter(string(out), "\n")
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
	ExternalsKnown bool
	Externals      []Repo
	Root           *Repo `json:"-"` // Don't include in json
}

func (repo *Repo) LoadExternals() error {
	rawExternals, err := execCmd(repo.Path, "git", "svn", "show-externals")
	if err != nil {
		return err
	}

	return repo.CookExternals(string(rawExternals))
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
					extPath := pathLib.Join(repo.Path, lastPath[1], match[2])
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

func contains(haystack [][]byte, needle []byte) bool {
	for _, e := range haystack {
		if bytes.Equal(e, needle) {
			return true
		}
	}

	return false
}

func (repo *Repo) ignoreExternalsAddMethod() {
	// Convert externals to relative path bytes
	externPaths := make([][]byte, 0, len(repo.Externals))
	for _, ext := range repo.Externals {
		relPath, err := filepath.Rel(repo.Path, ext.Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error converting external path:", err)
			continue
		}

		externPaths = append(externPaths, []byte(relPath))
	}

	var lines [][]byte
	ignoreFilename := pathLib.Join(repo.Path, ignoreRelPath)
	b, err := ioutil.ReadFile(ignoreFilename)
	if err != nil {
		if os.IsNotExist(err) {
		} else {
			fmt.Fprintln(os.Stderr, "Read:", err)
			return
		}
	} else {
		lines = bytes.Split(b, []byte{'\n'})
	}

	addBuf := new(bytes.Buffer)

	// The file is searched once for each externPath
	for _, externPath := range externPaths {
		if !contains(lines, externPath) {
			fmt.Fprintln(addBuf, string(externPath))
		}
	}

	if addBuf.Len() > 0 {
		f, err := os.OpenFile(ignoreFilename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer f.Close()

		_, err = addBuf.WriteTo(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}
}

func (repo *Repo) ignoreExternalsSubtractMethod() {
	externsToAdd := make(map[string]bool, len(repo.Externals))
	for _, ext := range repo.Externals {
		relPath, err := filepath.Rel(repo.Path, ext.Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error converting external path:", err)
			continue
		}

		externsToAdd[relPath] = true
	}

	f, err := os.OpenFile(pathLib.Join(repo.Path, ignoreRelPath),
		os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		fmt.Fprintln(os.Stderr, "IgnoreExternals:", err)
		return
	}
	defer f.Close()

	bufin := bufio.NewReader(f)
	for {
		ignore, err := bufin.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "IgnoreExternals:", err)
			}
			break
		}

		if externsToAdd[ignore] {
			// The extern is already ignored.
			delete(externsToAdd, ignore)
		}
	}

	for k := range externsToAdd {
		fmt.Fprintln(f, k)
	}
}

func (repo *Repo) IgnoreExternals() {
	if len(repo.Externals) == 0 {
		return // Nothing to do
	}

	// Add method: Is extern not in ignores? Add it!
	// Subtract method: Is ignore an extern? Remove it from the add list.
	const addMethod = false
	if addMethod {
		repo.ignoreExternalsAddMethod()
	} else {
		repo.ignoreExternalsSubtractMethod()
	}
}

func (repo *Repo) IgnoreAllExternals() {
	repo.IgnoreExternals()
	for _, ext := range repo.Externals {
		ext.IgnoreAllExternals()
	}
}

// Link externals to a root repo
func LinkTo(externs []Repo, root *Repo) {
	for i := range externs {
		externs[i].Root = root
		LinkTo(externs[i].Externals, root)
	}
}

// Link Root of all repos in the tree to the root repo.
func (repo *Repo) LinkRoot() {
	repo.Root = repo
	LinkTo(repo.Externals, repo)
}

func RewritePaths(repo *Repo, from, to string) {
	repo.Path = strings.Replace(repo.Path, from, to, 1)
	for i := range repo.Externals {
		RewritePaths(&repo.Externals[i], from, to)
	}
}

func appendCheckoutArgs(args []string, repoUrl string, askForArgs bool) []string {
	if askForArgs {
		fmt.Printf("Provide checkout args for %s:\n> ", repoUrl)
		buf := bufio.NewReader(os.Stdin)
		in, err := buf.ReadString('\n')
		in = strings.TrimSpace(in)
		if err == nil && in != "" {
			args = append(args, strings.Split(in, " ")...)
		}
	} else {
		args = append(args, strings.Split(defaultCheckoutArgs, " ")...)
	}

	return args
}

func gitClone(gitSrc, destDir string, askForArgs bool) (repo *Repo, err error) {
	err = os.MkdirAll(destDir, 0770)
	if err != nil {
		err = fmt.Errorf("Creating %s: %s", destDir, err)
		return
	}

	cmds := [][]string{
		[]string{"git init"},
		[]string{strings.Join([]string{"git remote add origin", gitSrc}, " ")},
		[]string{"git config --replace-all remote.origin.fetch"},
		[]string{"git config --add remote.origin.fetch +refs/notes/*:refs/notes/*"},
		[]string{"git fetch}"},
		[]string{"git config --remote-section remote.origin"},
		[]string{"git checkout -b master FETCH_HEAD"},
	}

	for _, cmd := range cmds {
		err = execCmdAttached(destDir, cmd[0], cmd[1:]...)
		if err != nil {
			os.RemoveAll(destDir)
			err = fmt.Errorf("%s: %s", strings.Join(cmd, " "), err)
			return
		}
	}

	repo, err = LoadConfig(destDir)
	if err != nil {
		// TODO: generate a config instead of erroring.
		err = fmt.Errorf("%s\nError loading gish config for cloned repo %s. Clone incomplete.", err, destDir)
		return
	}

	bork
	// The git clone process has to be done for each repo, though the config step only happens for the top one.

	// "git svn init", svnSrc}, " ")},
	for _, cmd := range cmds {
		err = execCmdAttached(destDir, cmd[0], cmd[1:]...)
		if err != nil {
			os.RemoveAll(destDir)
			err = fmt.Errorf("%s: %s", strings.Join(cmd, " "), err)
			return
		}
	}

	repo.Foreach([]string{"svn", "rebase"})
}

func svnClone(svnSrc, destDir string, askForArgs bool) (*Repo, error) {
	repo := &Repo{Path: destDir, Url: svnSrc}
	// The root member of the root repo points to itself.
	// Code can always jump through the root pointer to get to the root.
	// Recursive code will have to test or have separate initial/root functions.
	repo.Root = repo

	err := repo.SvnClone(askForArgs)
	return repo, err
}

func (repo *Repo) SvnClone(askForArgs bool) error {
	repoPath, repoDir := pathLib.Split(repo.Path)

	fmt.Printf("Cloning %q from svn url %q\n", repo.Path, repo.Url)
	err := os.MkdirAll(repo.Path, 0770)
	if err != nil {
		return err
	}

	args := []string{"svn", "clone"}
	args = appendCheckoutArgs(args, repo.Url, askForArgs)
	args = append(args, repo.Url, repoDir)
	err = execCmdAttached(repoPath, "git", args...)
	if err != nil {
		return err
	}

	if !repo.ExternalsKnown {
		err := repo.LoadExternals()
		if err != nil {
			return err
		} else {
			repo.IgnoreExternals()
		}
	}

	// Save the externals
	repo.WriteConfig()

	for i := range repo.Externals {
		err := repo.Externals[i].SvnClone(askForArgs)
		if err != nil {
			return err
		}
	}

	return nil
}

// Do a 'git clean' on each repo, removing the externals from the list.
func (repo *Repo) Clean() error {
	fmt.Fprintln(os.Stderr, "Cleaning repo ", repo.Path)

	toRmStr, err := execCmd(repo.Path, "git", "clean", "-ndx")
	if err != nil {
		return err
	}

	// Build a map of the externs
	extMap := make(map[string]bool, len(repo.Externals))
	for _, ext := range repo.Externals {
		extRelPath := strings.Trim(strings.Replace(ext.Path, repo.Path, "", 1), "/")
		extMap[extRelPath] = true
	}

	toRm := strings.Split(string(toRmStr), "\n")
	for i := range toRm {
		r := strings.Replace(toRm[i], "Would remove ", "", 1)
		r = strings.Trim(r, "/")

		if r == "" {
			continue
		}

		qualifiedR := pathLib.Join(repo.Path, r)

		if !extMap[r] {
			if !dryRun {
				err = os.RemoveAll(qualifiedR)
				if err != nil {
					fmt.Fprintln(os.Stdout, err)
				}
			} else {
				fmt.Printf("Would remove %q\n", qualifiedR)
			}
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

func (repo *Repo) Foreach(cmdLineArgs []string) error {
	paths := repo.Paths()
	for _, path := range paths {
		fmt.Printf("Repo %s:\n", path)
		err := execCmdAttached(path, "git", cmdLineArgs...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Git returned error:", err)
			// Don't quit, commands that get paged will return error.
		}
	}
}

// Write the repo configuration to file.
func (repo *Repo) WriteConfig() error {
	if repo.Root != repo {
		return repo.Root.WriteConfig()
	}

	b, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		return err
	}

	if !persistWithGitNotes {
		return ioutil.WriteFile(pathLib.Join(repo.Path, gishCachePathV2), b, 0660)
	}

	return GitNoteAdd(repo.Path, b)
}

// Create a Repo from a config file at the given location.
func LoadConfig(path string) (repo *Repo, err error) {
	if !IsDir(path) {
		return nil, fmt.Errorf("Config path is not a directory: %s", path)
	}

	b, err := ReadConfigV3(path)
	if err != nil {
		b, err = ReadConfigV2(path)
		if err != nil {
			return nil, fmt.Errorf("Unable to load gish config.")
		}
	}

	repo = &Repo{}
	err = json.Unmarshal(b, repo)

	repo.LinkRoot()

	return repo, err
}

func ReadConfigV3(path string) ([]byte, error) {
	// List the notes
	notedObj, err := GitLookupLatestGishNote(path)
	if err != nil {
		return []byte{}, fmt.Errorf("config note lookup: %s", err)
	}

	b, err := execGishNotes("show", notedObj)
	if err != nil {
		err = fmt.Errorf("config note show: %s", err)
	}

	return b, err
}

func ReadConfigV2(path string) ([]byte, error) {
	cachePath := pathLib.Join(path, gishCachePathV2)
	return ioutil.ReadFile(cachePath)
}

func Clone(cloneArgs []string) (*Repo, error) {
	flags := flag.NewFlagSet("clone", flag.ExitOnError)
	var svnSrc string
	var askForArgs bool
	flags.StringVar(&svnSrc, "s", "", "URL to svn repo that will be cloned.")
	flags.BoolVar(&askForArgs, "i", false, "Interactively prompt for clone arguments.")
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage:\n\tgish clone [-i] [-s=svnUrl | gitUrl] <destDir>\n")
		fmt.Fprint(os.Stderr, "\tClone the repo at the path or url svnUrl or gitUrl into destDir.\n")
		fmt.Fprintf(os.Stderr, "\tThe default clone arguments are '%s'\n", defaultCheckoutArgs)

		fmt.Fprint(os.Stderr, "Options:\n")
		flags.PrintDefaults()
	}

	// DELME
	// TODO: these aren't supported yet
	// Update/subclone:
	// 'gish clone' in a repo
	// 'gish clone trunk' where trunk is repo
	// If no args and pwd IsRepo or no URL and destDir IsRepo, update it

	// Clone git-svn repo
	// 'gish clone trunk cloneOfTrunk'

	// ^^
	//   Is the given url a git repo?
	//   Is there a gish config file?
	//   Clone top down.

	//     If the gish config were stored in the repo it could:
	//        be versioned
	//        be retrieved remotely
	/*
	   $ blob=$(git hash-object -w a.out)
	   $ git notes --ref=built add -C "$blob" HEAD

	   GIT_NOTES_REF=refs/notes/gish git notes add
	   GIT_NOTES_REF=refs/notes/gish git notes show


	   ----
	   To clone, do a normal clone then add this to the origin ref and fetch
	   fetch = +refs/notes/*:refs/notes/*

	   ***this might get rid of the alt-config method
	*/

	/*
	   if len(cmdLineArgs) < 2 {
	       UsageExit(flags.Usage, "Not enough arguments to 'gish clone'.")
	   }
	*/

	flags.Parse(cloneArgs)
	nonFlagArgs := flags.Args()

	var srcUrl string
	var destDir string

	if len(nonFlagArgs) < 1 {
		UsageExit(flags.Usage, "Not enough arguments to 'gish clone'.")
	} else if len(nonFlagArgs) == 1 {
		if svnSrc == "" {
			UsageExit(flags.Usage, "Provide source repo and destDir.")
		} else {
			srcUrl = svnSrc
			destDir = strings.TrimSpace(nonFlagArgs[0])
		}
	} else if len(nonFlagArgs) == 2 {
		if svnSrc != "" {
			UsageExit(flags.Usage, "Provide only one source repo url.")
		} else {
			srcUrl = strings.TrimSpace(nonFlagArgs[0])
			destDir = strings.TrimSpace(nonFlagArgs[1])
		}
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		UsageExit(flags.Usage, fmt.Sprintf("invalid destdir %s: %v", destDir, err))
	}
	_, err = os.Stat(absDestDir)
	if err == nil {
		UsageExit(flags.Usage, "destDir exists")
	}

	// DELME
	/*
	   // Fill in the url provided, clone will fill the rest
	   // This check may not be worth much. Apparently "-i=false" is a valid url.
	   svnUrl, err := url.Parse(strings.TrimSpace(nonFlagArgs[0]))
	   if err != nil {
	       UsageExit(flags.Usage, fmt.Sprint("Error parsing svn Url:", err.Error()))
	   }

	   var destDir string
	   if len(nonFlagArgs) == 2 {
	       destDir = nonFlagArgs[1]
	   } else {
	       pathParts := strings.Split(svnUrl.Path, "/")
	       destDir = pathParts[len(pathParts)-1]
	   }
	*/

	if svnSrc == "" {
		return gitClone(srcUrl, absDestDir, askForArgs)
	} else {
		return svnClone(srcUrl, absDestDir, askForArgs)
	}

}

func NewRepo(cmdLineArgs []string) (*Repo, error) {
	rootPath, err := FindRootRepoPath()
	if err != nil {
		return nil, err
	}

	if repo, err := LoadConfig(rootPath); err == nil {
		repo.Root = repo
		// Ensure the Repo path points to the directory containing the git-svn repo
		RewritePaths(repo, repo.Path, rootPath)

		return repo, nil
	} else {
		fmt.Println(err)
	}

	// LoadConfig failed, create a repo from git
	fmt.Printf("Loading info from git. This may take a while.\n")
	url, err := GitSvnInfo(rootPath, "URL")
	if err != nil {
		return nil, err
	}

	repo := &Repo{Path: rootPath, Url: url}
	repo.Root = repo

	err = repo.LoadExternals()
	if err != nil {
		return nil, err
	}

	return repo, nil
}

func cmdClean(args []string, repo *Repo) {
	flags := flag.NewFlagSet("clean", flag.ExitOnError)
	flags.BoolVar(&dryRun, "n", false, "List the files that would be removed.")
	flags.BoolVar(&force, "f", false, "Enable file removal. Like git, -n or -f is required for clean.")
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage:\n\tgish clean [options]\n")
		fmt.Fprint(os.Stderr, "Options:\n")
		flags.PrintDefaults()
	}

	if len(args) < 2 {
		UsageExit(flags.Usage, "Not enough arguments to 'gish clean'.")
	}

	flags.Parse(args[1:])

	if !force && !dryRun {
		UsageExit(flags.Usage, "-n or -f required for clean.")
	}

	repo.Clean()
}

func main() {
	flag.Usage = Usage
	flag.Parse()

	cmdLineArgs := flag.Args()
	if len(cmdLineArgs) == 0 {
		UsageExit(Usage, "No command provided.")
	}

	var repo *Repo
	var err error

	if cmdLineArgs[0] == "clone" {
		repo, err = Clone(cmdLineArgs[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error cloning:", err)
			os.Exit(1)
		}
	} else {
		repo, err = NewRepo(cmdLineArgs)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		switch cmdLineArgs[0] {
		case "list":
			repo.List()
		case "clean":
			cmdClean(cmdLineArgs, repo)
		case "updateignores":
			repo.IgnoreAllExternals()
		default:
			repo.Foreach(cmdLineArgs)
		}
	}

	err = repo.WriteConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error writing config: ", err)
	}

}
