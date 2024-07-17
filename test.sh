#!/bin/bash
set -euo pipefail

rm -rf _test || true
mkdir _test

>&2 git init _test --object-format=sha256
(cd _test
	{
		echo "a file" > a_file
		mkdir a_dir
		echo "second file" > a_dir/other_file
		echo "more file" > a_dir/more_files
		mkdir a_dir/deeper
		echo "more file" > a_dir/deeper/samefile
		ln -s "target string" a_symlink
		git add .
		git commit -m "demo sha256" # don't care about commit hash, but is easiest route through CLI to get treehash.
	} >&2
	git ls-tree HEAD:a_dir
	git ls-tree HEAD
	git cat-file HEAD -p | head -n1 | awk '{print $2}'
)

echo

rm -rf _test/.git
go run gittreehash.go _test/a_dir/other_file
go run gittreehash.go _test/a_dir
go run gittreehash.go _test/a_file
go run gittreehash.go _test/a_symlink
go run gittreehash.go _test
