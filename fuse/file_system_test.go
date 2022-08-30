package fuse_test

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/internal/testingutil"
)

func TestFileSystem_OK(t *testing.T) {
	for _, mode := range []string{"DELETE"} { // TODO: TRUNCATE, PERSIST, WAL
		t.Run(mode, func(t *testing.T) {
			fs := newOpenFileSystem(t)
			dsn := filepath.Join(fs.Path(), "db")
			db := testingutil.OpenSQLDB(t, dsn)

			// Set the journaling mode.
			if _, err := db.Exec(`PRAGMA journal_mode = ` + mode); err != nil {
				t.Fatal(err)
			}

			// Create a simple table with a single value.
			if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
				t.Fatal(err)
			} else if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
				t.Fatal(err)
			}

			// Ensure we can retrieve the data back from the database.
			var x int
			if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
				t.Fatal(err)
			} else if got, want := x, 100; got != want {
				t.Fatalf("x=%d, want %d", got, want)
			}

			// Verify transaction count.
			if got, want := fs.Store().DB(1).TXID(), uint64(2); got != want {
				t.Fatalf("txid=%d, want %d", got, want)
			}

			// Close & reopen.
			testingutil.ReopenSQLDB(t, &db, dsn)
			db = testingutil.OpenSQLDB(t, dsn)
			if _, err := db.Exec(`INSERT INTO t VALUES (200)`); err != nil {
				t.Fatal(err)
			}

			// Ensure we can retrieve the data back from the database.
			rows, err := db.Query(`SELECT x FROM t`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()

			var values []int
			for rows.Next() {
				var x int
				if err := rows.Scan(&x); err != nil {
					t.Fatal(err)
				}
				values = append(values, x)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}

			if got, want := values, []int{100, 200}; !reflect.DeepEqual(got, want) {
				t.Fatalf("values=%d, want %d", got, want)
			}

			// Verify new transaction count.
			if got, want := fs.Store().DB(1).TXID(), uint64(3); got != want {
				t.Fatalf("txid=%d, want %d", got, want)
			}
		})
	}
}

// Ensure LiteFS prevents a database from enabling WAL mode on a database.
func TestFileSystem_PreventWAL(t *testing.T) {
	fs := newOpenFileSystem(t)
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Set the journaling mode.
	var x string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&x); err == nil || err.Error() != `disk I/O error` {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestFileSystem_Rollback(t *testing.T) {
	fs := newOpenFileSystem(t)
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	// Attempt to insert data but roll it back.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	} else if _, err := tx.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	} else if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	var x int
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != sql.ErrNoRows {
		t.Fatalf("expected no rows (%#v)", err)
	}
}

func TestFileSystem_NoWrite(t *testing.T) {
	fs := newOpenFileSystem(t)
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if got, want := fs.Store().DB(1).TXID(), uint64(1); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Start and commit a transaction without a write.
	if _, err := db.Exec(`BEGIN IMMEDIATE; COMMIT`); err != nil {
		t.Fatal(err)
	}

	// Ensure the transaction ID has not incremented.
	if got, want := fs.Store().DB(1).TXID(), uint64(1); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}
}

func TestFileSystem_MultipleJournalSegments(t *testing.T) {
	fs := newOpenFileSystem(t)
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)
	const rowN = 1000

	// Ensure cache size is low so we get multiple segments flushed.
	if _, err := db.Exec(`PRAGMA cache_size = 10`); err != nil {
		t.Fatal(err)
	}

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT)`); err != nil {
		t.Fatal(err)
	}

	// Create rows that span over many pages.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 1; i <= rowN; i++ {
		if _, err := tx.Exec(`INSERT INTO t (id, x) VALUES (?, ?)`, i, strings.Repeat(fmt.Sprintf("%08x", i), 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Create a transaction with large values so it creates a lot of pages.
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 1; i <= rowN; i++ {
		if _, err := tx.Exec(`UPDATE t SET x =? WHERE id = ?`, strings.Repeat(fmt.Sprintf("%04x", i), 100), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Ensure the transaction ID has not incremented.
	if got, want := fs.Store().DB(1).TXID(), uint64(3); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Verify database integrity
	if _, err := db.Exec(`PRAGMA integrity_check`); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_ReadDir(t *testing.T) {
	fs := newOpenFileSystem(t)
	db0 := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db0"))
	db1 := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db1"))

	if _, err := db0.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db1.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	// Read directory listing from mount.
	cmd := exec.Command("ls")
	cmd.Dir = fs.Path()
	if buf, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err)
	} else if got, want := string(buf), "db0\ndb1\n"; got != want {
		t.Fatalf("unexpected output: %q", got)
	}
}

// Ensures the statfs() executes and does not panic.
func TestFileSystem_Statfs(t *testing.T) {
	fs := newOpenFileSystem(t)
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(fs.Path(), &statfs); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_Pos(t *testing.T) {
	t.Run("ReopenHandle", func(t *testing.T) {
		fs := newOpenFileSystem(t)
		dsn := filepath.Join(fs.Path(), "db")
		db := testingutil.OpenSQLDB(t, dsn)

		// Write a transaction & verify the position.
		if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
			t.Fatal(err)
		}
		if buf, err := os.ReadFile(dsn + "-pos"); err != nil {
			t.Fatal(err)
		} else if got, want := string(buf), "0000000000000001/a3b2b72f1147c9bc\n"; got != want {
			t.Fatalf("pos=%q, want %q", got, want)
		}

		// Write another transaction & ensure position changes.
		if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
			t.Fatal(err)
		}
		if buf, err := os.ReadFile(dsn + "-pos"); err != nil {
			t.Fatal(err)
		} else if got, want := string(buf), "0000000000000002/e2e79e6905b952db\n"; got != want {
			t.Fatalf("pos=%q, want %q", got, want)
		}
	})

	t.Run("ReuseHandle", func(t *testing.T) {
		fs := newOpenFileSystem(t)
		dsn := filepath.Join(fs.Path(), "db")
		db := testingutil.OpenSQLDB(t, dsn)

		// Write a transaction,
		if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
			t.Fatal(err)
		}

		// Open the "pos" handle once and reuse it.
		posFile, err := os.Open(dsn + "-pos")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = posFile.Close() }()

		buf := make([]byte, fuse.PosFileSize)
		if _, err := posFile.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		} else if got, want := string(buf), "0000000000000001/a3b2b72f1147c9bc\n"; got != want {
			t.Fatalf("pos=%q, want %q", got, want)
		}

		// Write another transaction & ensure position changes.
		if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
			t.Fatal(err)
		}
		if _, err := posFile.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		} else if got, want := string(buf), "0000000000000002/e2e79e6905b952db\n"; got != want {
			t.Fatalf("pos=%q, want %q", got, want)
		}
	})
}

func TestFileSystem_MultipleTx(t *testing.T) {
	fs := newOpenFileSystem(t)
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Start with some data.
	if _, err := db.Exec(`PRAGMA busy_timeout = 2000`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Start a read transaction.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	var x int
	if err := tx.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	}

	// Try to write at the same time.
	ch := make(chan error)
	go func() {
		_, err := db.Exec(`INSERT INTO t VALUES (200)`)
		ch <- err
	}()

	// Wait for a moment & rollback.
	time.Sleep(1 * time.Second)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Wait for write transaction.
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
}

func newFileSystem(tb testing.TB) *fuse.FileSystem {
	tb.Helper()

	path := tb.TempDir()
	store := litefs.NewStore(filepath.Join(path, ".mnt"), true)
	store.Leaser = litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202")
	if err := store.Open(); err != nil {
		tb.Fatalf("cannot open store: %s", err)
	}

	fs := fuse.NewFileSystem(filepath.Join(path, "mnt"), store)
	if err := os.MkdirAll(fs.Path(), 0777); err != nil {
		tb.Fatalf("cannot create mount point: %s", err)
	}
	fs.Debug = *debug

	store.Invalidator = fs

	return fs
}

func newOpenFileSystem(tb testing.TB) *fuse.FileSystem {
	tb.Helper()

	fs := newFileSystem(tb)
	if err := fs.Mount(); err != nil {
		tb.Fatalf("cannot open file system: %s", err)
	}

	tb.Cleanup(func() {
		if err := fs.Unmount(); err != nil {
			tb.Errorf("server close failed: %s", err)
		} else if err := os.RemoveAll(fs.Path()); err != nil {
			tb.Errorf("mount path removal failed: %s", err)
		} else if err := os.RemoveAll(fs.Store().Path()); err != nil {
			tb.Errorf("data path removal failed: %s", err)
		}
	})

	return fs
}
