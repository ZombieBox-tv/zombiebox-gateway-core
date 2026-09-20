//go:build !android && !sqlite_cgo

package store

import _ "modernc.org/sqlite"

const driverName = "sqlite"
