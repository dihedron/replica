package main

import (
	"github.com/dihedron/replica/workflow"
)

type Command struct {
	DatabaseURL string `short:"d" long:"database-url" description:"The URL of the database holding durable execution data." optional:"yes" env:"REPLICA_DATABASE_URL" default:"postgres://postgres:dbos@localhost:5432/postgres"`
	Application string `short:"a" long:"application-name" description:"The name of the application workflow." optional:"yes" env:"REPLICA_APPLICATION" default:"my-durable-app"`
}

// Execute executes the command.
func (cmd *Command) Execute(args []string) error {
	return workflow.ExecuteGeminiWorkflow(cmd.DatabaseURL, cmd.Application, args)
}
