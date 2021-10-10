#!/bin/bash

export PROJECT_NAME="macro-guard"

set -e

PLATFORM="macOS"
CLANG="clang++"
GO="go"
TIME="time"
TIME_TOTAL="time"

if [[ -f /proc/version ]]; then
  if grep -q Linux /proc/version; then
    PLATFORM="lin"
    TIME="time --format=%es\n"
    TIME_TOTAL="time --format=total\t%es\n"
  fi
  if grep -q Microsoft /proc/version; then
    PLATFORM="win"
    CMAKE="cmake.exe"
    CLANG_FORMAT="clang-format.exe"
  fi
fi
CLANG="$TIME $CLANG"
GO="$TIME $GO"

case "$1" in
  # Desktop
  release)
    mkdir -p build
    $GO build gx.go
    rm -rf build/example.gx.*
    $TIME ./gx ./example build/example
    rm gx
    if [[ -f build/example.gx.cc ]]; then
      $CLANG -std=c++20 -Wall -O3 -Iexample -o output build/example.gx.cc || true
    fi
    if [[ -f output ]]; then
      ./output || true
      rm output
    fi
    exit 1
    ;;
esac
