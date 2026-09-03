#!/usr/bin/env bash
# Go lives in the conda env "gotmon", holding the official Go distribution
# rather than the conda-forge package: this machine's DLP encrypts .h files
# written by trusted processes, and conda (being trusted) encrypted Go's own
# assembly headers, which broke every build. See docs/BUILD.md.
#
# GOROOT is given in Windows form with forward slashes rather than an MSYS
# /c/... path. go.exe is a Windows binary and cannot read the MSYS form, and
# relying on MSYS to translate it breaks the moment anything in the shell sets
# MSYS_NO_PATHCONV, which some test commands need.
CONDA_ENV_PREFIX="${CONDA_ENV_PREFIX:-C:/HUIXIN/AppData/Anaconda/envs/gotmon}"
export GOROOT="$CONDA_ENV_PREFIX/go"
export GO="$GOROOT/bin/go.exe"
export GOFMT="$GOROOT/bin/gofmt.exe"
