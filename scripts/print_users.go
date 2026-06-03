//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/usezoracle/rails-sui/config"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	client := storage.GetClient()
	defer client.Close()

	ctx := context.Background()
	for _, email := range []string{"ada@example.com", "ada2@example.com"} {
		user, err := client.User.Query().Where(userEnt.EmailEQ(email)).Only(ctx)
		if err != nil {
			fmt.Printf("User %s query error: %v\n", email, err)
			continue
		}
		fmt.Printf("User %s: ID=%s, FirstName=%s, LastName=%s, Scope=%s, IsEmailVerified=%v, PasswordHash=%s\n",
			email, user.ID, user.FirstName, user.LastName, user.Scope, user.IsEmailVerified, user.Password)
	}
}
