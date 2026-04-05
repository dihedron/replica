package step

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// --- Domain Models ---
type User struct {
	ID    string
	Email string
}

func CreateUserInDB(ctx context.Context, email string) (User, error) {
	fmt.Printf("Action: Creating user record for %s...\n", email)
	// Simulate a successful database insert
	return User{ID: "USR-999", Email: email}, nil
}

func CreateBillingProfile(ctx context.Context, userID string) (string, error) {
	fmt.Printf("Action: Creating billing profile for %s...\n", userID)
	// Simulate a successful API call to Stripe
	return "CUST-12345", nil
}

func SendWelcomeEmail(ctx context.Context, email string) (string, error) {
	fmt.Printf("Action: Sending welcome email to %s...\n", email)
	// Simulate a failure (e.g., email provider rejected the address)
	return "", errors.New("fatal error: email provider rejected the address")
}

// --- Compensation Steps (The "Undos") ---

func DeleteUserFromDB(ctx context.Context, userID string) (string, error) {
	fmt.Printf("Compensation: Deleting user %s from database...\n", userID)
	return "User deleted", nil
}

func DeleteBillingProfile(ctx context.Context, billingID string) (string, error) {
	fmt.Printf("Compensation: Deleting billing profile %s...\n", billingID)
	return "Billing profile deleted", nil
}

// Workflows orchestrate steps. They take a special dbos.DBOSContext.
func UserRegistrationSaga(ctx dbos.DBOSContext, email string) (string, error) {
	slog.Debug("Starting registration saga", "email", email)

	// Step 1: Create User
	user, err := dbos.RunAsStep(
		ctx,
		func(c context.Context) (User, error) { return CreateUserInDB(c, email) },
		dbos.WithStepName("STEP: Create User"),
	)
	if err != nil {
		// Nothing to undo yet.
		return "", fmt.Errorf("registration failed at user creation: %w", err)
	}

	// Step 2: Create Billing Profile
	billingID, err := dbos.RunAsStep(
		ctx,
		func(c context.Context) (string, error) { return CreateBillingProfile(c, user.ID) },
		dbos.WithStepName("STEP: Create Billing Profile"),
	)
	if err != nil {
		fmt.Println("Billing creation failed! Rolling back user...")

		// Compensation: Undo Step 1
		dbos.RunAsStep(
			ctx,
			func(c context.Context) (string, error) { return DeleteUserFromDB(c, user.ID) },
			dbos.WithStepName("STEP: [Compensate] Delete User"),
			dbos.WithStepMaxRetries(3), // Retry compensations if the DB is temporarily locked!
		)

		return "", fmt.Errorf("registration failed at billing; user rolled back: %w", err)
	}

	// Step 3: Send Welcome Email
	_, err = dbos.RunAsStep(
		ctx,
		func(c context.Context) (string, error) { return SendWelcomeEmail(c, email) },
		dbos.WithStepName("STEP: Send Welcome Email"),
	)

	// Handle Failure for Step 3
	if err != nil {
		fmt.Println("Email failed! Initiating full rollback...")

		// Compensation: Undo Step 2
		dbos.RunAsStep(
			ctx,
			func(c context.Context) (string, error) { return DeleteBillingProfile(c, billingID) },
			dbos.WithStepName("STEP: [Compensate] Delete Billing Profile"),
			dbos.WithStepMaxRetries(3),
		)

		// Compensation: Undo Step 1
		dbos.RunAsStep(
			ctx,
			func(c context.Context) (string, error) { return DeleteUserFromDB(c, user.ID) },
			// Name must be unique to the call site in the workflow
			dbos.WithStepName("STEP: [Compensate] Delete User Phase 3"),
			dbos.WithStepMaxRetries(3),
		)

		return "", fmt.Errorf("registration failed at email step; all previous steps rolled back: %w", err)
	}

	return "User registered successfully!", nil
}
