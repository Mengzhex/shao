#!/usr/bin/env bash
# Exports GO, GOFMT and (only where it applies) GOROOT.
#
# On the development machine Go lives in the conda env "gotmon", holding the
# official Go distribution rather than the conda-forge package: this machine's
# DLP encrypts .h files written by trusted processes, and conda (being trusted)
# encrypted Go's own assembly headers, which broke every build. See
# docs/BUILD.md.
#
# GOROOT is given in Windows form with forward slashes rather than an MSYS
# /c/... path. go.exe is a Windows binary and cannot read the MSYS form, and
# relying on MSYS to translate it breaks the moment anything in the shell sets
# MSYS_NO_PATHCONV, which some test commands need.
#
# Everywhere else -- a CI runner, anyone else's machine -- that prefix does not
# exist, so nothing is exported but the names, pointing at whatever is on PATH.
# Exporting a GOROOT that does not exist is worse than exporting none: it
# breaks the toolchain that *is* installed, with "cannot find GOROOT directory"
# and exit 2. That is not hypothetical; it is how this was found, with a CI
# runner reporting "no usable Go toolchain" while Go sat on its PATH.
CONDA_ENV_PREFIX="${CONDA_ENV_PREFIX:-C:/HUIXIN/AppData/Anaconda/envs/gotmon}"

if [ -x "$CONDA_ENV_PREFIX/go/bin/go.exe" ]; then
	export GOROOT="$CONDA_ENV_PREFIX/go"
	export GO="$GOROOT/bin/go.exe"
	export GOFMT="$GOROOT/bin/gofmt.exe"
else
	export GO="${GO:-go}"
	export GOFMT="${GOFMT:-gofmt}"
fi
