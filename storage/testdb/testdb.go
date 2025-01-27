// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package testdb creates new databases for tests.
package testdb

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/google/trillian/testonly"

	_ "github.com/go-sql-driver/mysql" // mysql driver
)

var (
	trillianSQL   = testonly.RelativeToPackage("../mysql/schema/storage.sql")
	dataSourceURI = flag.String("test_mysql_uri", "root@tcp(127.0.0.1)/", "The MySQL uri to use when running tests")
)

// MySQLAvailable indicates whether a default MySQL database is available.
func MySQLAvailable() bool {
	db, err := sql.Open("mysql", *dataSourceURI)
	if err != nil {
		log.Printf("sql.Open(): %v", err)
		return false
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Printf("db.Ping(): %v", err)
		return false
	}
	return true
}

// newEmptyDB creates a new, empty database.
// It returns the database handle and a clean-up function, or an error.
// The returned clean-up function should be called once the caller is finished
// using the DB, the caller should not continue to use the returned DB after
// calling this function as it may, for example, delete the underlying
// instance.
func newEmptyDB(ctx context.Context) (*sql.DB, func(context.Context), error) {
	db, err := sql.Open("mysql", *dataSourceURI)
	if err != nil {
		return nil, nil, err
	}

	// Create a randomly-named database and then connect using the new name.
	name := fmt.Sprintf("trl_%v", time.Now().UnixNano())

	stmt := fmt.Sprintf("CREATE DATABASE %v", name)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return nil, nil, fmt.Errorf("error running statement %q: %v", stmt, err)
	}

	db.Close()
	db, err = sql.Open("mysql", *dataSourceURI+name)
	if err != nil {
		return nil, nil, err
	}

	done := func(ctx context.Context) {
		defer db.Close()
		if _, err := db.ExecContext(ctx, "DROP DATABASE %v", name); err != nil {
			glog.Warningf("Failed to drop test database %q: %v", name, err)
		}
	}

	return db, done, db.Ping()
}

// NewTrillianDB creates an empty database with the Trillian schema. The database name is randomly
// generated.
// NewTrillianDB is equivalent to Default().NewTrillianDB(ctx).
func NewTrillianDB(ctx context.Context) (*sql.DB, func(context.Context), error) {
	db, done, err := newEmptyDB(ctx)
	if err != nil {
		return nil, nil, err
	}

	sqlBytes, err := ioutil.ReadFile(trillianSQL)
	if err != nil {
		return nil, nil, err
	}

	for _, stmt := range strings.Split(sanitize(string(sqlBytes)), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, nil, fmt.Errorf("error running statement %q: %v", stmt, err)
		}
	}
	return db, done, nil
}

func sanitize(script string) string {
	buf := &bytes.Buffer{}
	for _, line := range strings.Split(string(script), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || strings.Index(line, "--") == 0 {
			continue // skip empty lines and comments
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return buf.String()
}

// SkipIfNoMySQL is a test helper that skips tests that require a local MySQL.
func SkipIfNoMySQL(t *testing.T) {
	t.Helper()
	if !MySQLAvailable() {
		t.Skip("Skipping test as MySQL not available")
	}
}
