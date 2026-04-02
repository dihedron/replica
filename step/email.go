package step

import (
	"context"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

func SendWelcomeEmail(name string, email string) dbos.Step[Result[string]] {
	// The closure captures 'name' and 'email' from the outer workflow scope
	return func(ctx context.Context) (Result[string], error) {
		// ... simulate an external API call ...
		fmt.Printf("Sending email to %s (ID: %s)...\n", email, name)
		return Result[string]{Data: "Email sent successfully"}, nil
	}
}
