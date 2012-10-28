gish - git-svn helper
=====================
Utility for management of git-svn repositories that contain externals.

* Clone: recursive clone of externals into an existing git-svn repository
* Clean: recursive git clean, doesn't remove external repo
* Execute git with command arguments within repo and its externals.

Usage
-----
All usage is documented in the tool. `gish -h` for command list, `gish <command> -h` for command help

### Clone
Clone the svn repo.
    `gish clone svn://svnserver/repo/path [destDir]`

Use a preexisting config file to create a new repo. This avoids fetching the externals from the svn server. The config file can be found in .git/info/gish.conf
    `gish clone -c=gish.conf destdir`

### Clean
Remove all untracked files. `-n` lists files that would be removed, `-f` enables removal. One flag must be provided.

Clone the root repo manually. Within that repo, `gish update` will recursively clone the externals. Normal git commands are performed on the root repo and all externals, recursively. For example, `gish status -uno` will show the status for all the repos, hiding the untracked files.

Installation
------------
Gish is written in go. The Go compiler is [simple to install](http://golang.org/doc/install). Once Go is installed, gish can be downloaded and installed using the go tool.

    `go get github.com/mikezuff/gish/`
    `go install github.com/mikezuff/gish/`

If you have problems with these commands, ensure that $GOPATH and $GOROOT are set properly and that $GOPATH/bin and $GOROOT/bin are in your $PATH. See the [Go installation instructions](http://golang.org/doc/install) for more info.

Known Issues
------------
Only the "^" svn relative external format is supported.

Thanks
------
Credit is due to the authors of these other tools for inspiring a Go implementation.

* [liyanage's git-tools](https://github.com/liyanage/git-tools/)
* [andrep's git-svn-clone-externals](https://github.com/andrep/git-svn-clone-externals)
