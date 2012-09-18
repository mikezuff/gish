gish - git-svn helper
====
Helper for management of git-svn repositories that contain externals. 

Usage
====
Clone the root repo manually. Within that repo, 'gish update' will recursively clone the externals.
Normal git commands are performed on the root repo and all externals, recursively. For example:
gish status -uno  # will show the status for all the repos, hiding the untracked files

Installation
====
go get github.com/mikezuff/gish/
go install github.com/mikezuff/gish/
This will compile gish and put it in your $GOPATH/bin directory. See http://golang.org/doc/install

Other tools
====
Other tools that operate on git-svn externals are available:
https://github.com/liyanage/git-tools/ 
https://github.com/andrep/git-svn-clone-externals 
