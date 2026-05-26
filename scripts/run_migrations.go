package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/usezoracle/rails-sui/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	DSN := config.DBConfig()
	db, err := sql.Open("pgx", DSN)
	if err != nil {
		panic(fmt.Errorf("failed to open database: %w", err))
	}
	defer db.Close()

	ctx := context.Background()

	// 1. Wipe the database schema cleanly by dropping and recreating 'public' schema
	fmt.Println("Wiping existing database schema...")
	_, err = db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO public;")
	if err != nil {
		panic(fmt.Errorf("failed to wipe schema: %w", err))
	}
	fmt.Println("Database schema wiped successfully.")

	// 2. Read migration files in /Users/mac/rails/ent/migrate/migrations
	migrationsDir := "/Users/mac/rails/ent/migrate/migrations"
	files, err := os.ReadDir(migrationsDir)
	if err != nil {
		panic(fmt.Errorf("failed to read migrations directory: %w", err))
	}

	var migrationFiles []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
			migrationFiles = append(migrationFiles, f.Name())
		}
	}

	// Sort migration files chronologically (by prefix timestamp)
	sort.Strings(migrationFiles)

	fmt.Printf("Found %d migration files to apply.\n", len(migrationFiles))

	// 3. Apply each migration file sequentially
	for _, filename := range migrationFiles {
		filePath := filepath.Join(migrationsDir, filename)
		fmt.Printf("Applying migration: %s...\n", filename)

		contentBytes, err := os.ReadFile(filePath)
		if err != nil {
			panic(fmt.Errorf("failed to read migration file %s: %w", filename, err))
		}

		sqlContent := string(contentBytes)
		if strings.TrimSpace(sqlContent) == "" {
			fmt.Printf("Skipping empty migration: %s\n", filename)
			continue
		}

		// Execute migration SQL statements
		_, err = db.ExecContext(ctx, sqlContent)
		if err != nil {
			panic(fmt.Errorf("failed to execute migration %s: %w\nSQL:\n%s", filename, err, sqlContent))
		}
	}

	fmt.Println("All migrations applied successfully!")
}
