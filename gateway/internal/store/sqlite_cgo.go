//go:build android || sqlite_cgo

package store

// Gateway Edge is built natively with Termux's Bionic toolchain. This driver
// never enters the Android client APK. sqlite_cgo also allows host verification.
import _ "github.com/mattn/go-sqlite3"

const driverName = "sqlite3"
