package main

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/github/ghcs/api"
	"github.com/github/ghcs/internal/codespaces"
	"github.com/github/go-liveshare"
	"github.com/spf13/cobra"
)

func NewSSHCmd() *cobra.Command {
	var sshProfile string

	sshCmd := &cobra.Command{
		Use:   "ssh",
		Short: "ssh",
		Long:  "ssh",
		RunE: func(cmd *cobra.Command, args []string) error {
			return SSH(sshProfile)
		},
	}

	sshCmd.Flags().StringVarP(&sshProfile, "profile", "", "", "SSH Profile")

	return sshCmd
}

func init() {
	rootCmd.AddCommand(NewSSHCmd())
}

func SSH(sshProfile string) error {
	apiClient := api.New(os.Getenv("GITHUB_TOKEN"))
	ctx := context.Background()

	user, err := apiClient.GetUser(ctx)
	if err != nil {
		return fmt.Errorf("error getting user: %v", err)
	}

	codespace, err := codespaces.ChooseCodespace(ctx, apiClient, user)
	if err != nil {
		if err == codespaces.ErrNoCodespaces {
			fmt.Println(err.Error())
			return nil
		}

		return fmt.Errorf("error choosing codespace: %v", err)
	}

	token, err := apiClient.GetCodespaceToken(ctx, user.Login, codespace.Name)
	if err != nil {
		return fmt.Errorf("error getting codespace token: %v", err)
	}

	liveShareClient, err := codespaces.ConnectToLiveshare(ctx, apiClient, token, codespace)
	if err != nil {
		return fmt.Errorf("error connecting to liveshare: %v", err)
	}

	terminal, err := liveShareClient.NewTerminal()
	if err != nil {
		return fmt.Errorf("error creating liveshare terminal: %v", err)
	}

	fmt.Println("Preparing SSH...")
	if sshProfile == "" {
		containerID, err := getContainerID(ctx, terminal)
		if err != nil {
			return fmt.Errorf("error getting container id: %v", err)
		}

		if err := setupSSH(ctx, terminal, containerID, codespace.RepositoryName); err != nil {
			return fmt.Errorf("error creating ssh server: %v", err)
		}
	}

	server, err := liveShareClient.NewServer()
	if err != nil {
		return fmt.Errorf("error creating server: %v", err)
	}

	rand.Seed(time.Now().Unix())
	port := rand.Intn(9999-2000) + 2000 // improve this obviously
	if err := server.StartSharing(ctx, "sshd", 2222); err != nil {
		return fmt.Errorf("error sharing sshd port: %v", err)
	}

	portForwarder := liveshare.NewLocalPortForwarder(liveShareClient, server, port)
	go func() {
		if err := portForwarder.Start(ctx); err != nil {
			panic(fmt.Errorf("error forwarding port: %v", err))
		}
	}()

	connectDestination := sshProfile
	if connectDestination == "" {
		connectDestination = fmt.Sprintf("%s@localhost", getSSHUser(codespace))
	}

	fmt.Println("Ready...")
	if err := connect(ctx, port, connectDestination); err != nil {
		return fmt.Errorf("error connecting via SSH: %v", err)
	}

	return nil
}

func connect(ctx context.Context, port int, destination string) error {
	cmd := exec.CommandContext(ctx, "ssh", destination, "-C", "-p", strconv.Itoa(port), "-o", "NoHostAuthenticationForLocalhost=yes")
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func getContainerID(ctx context.Context, terminal *liveshare.Terminal) (string, error) {
	cmd := terminal.NewCommand(
		"/",
		"/usr/bin/docker ps -aq --filter label=Type=codespaces --filter status=running",
	)
	stream, err := cmd.Run(ctx)
	if err != nil {
		return "", fmt.Errorf("error running command: %v", err)
	}

	scanner := bufio.NewScanner(stream)
	scanner.Scan()

	containerID := scanner.Text()
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error scanning stream: %v", err)
	}

	if err := stream.Close(); err != nil {
		return "", fmt.Errorf("error closing stream: %v", err)
	}

	return containerID, nil
}

func setupSSH(ctx context.Context, terminal *liveshare.Terminal, containerID, repositoryName string) error {
	getUsernameCmd := "GITHUB_USERNAME=\"$(jq .CODESPACE_NAME /workspaces/.codespaces/shared/environment-variables.json -r | cut -f1 -d -)\""
	makeSSHDirCmd := "mkdir /home/codespace/.ssh"
	getUserKeysCmd := "curl --silent --fail \"https://github.com/$(echo $GITHUB_USERNAME).keys\" > /home/codespace/.ssh/authorized_keys"
	setupSecretsCmd := `cat /workspaces/.codespaces/shared/.user-secrets.json | jq -r ".[] | select (.type==\"EnvironmentVariable\") | .name+\"=\"+.value" > /home/codespace/.zshenv`
	setupLoginDirCmd := fmt.Sprintf("echo \"cd /workspaces/%v; exec /bin/zsh;\" > /home/codespace/.bash_profile", repositoryName)

	compositeCommand := []string{getUsernameCmd, makeSSHDirCmd, getUserKeysCmd, setupSecretsCmd, setupLoginDirCmd}
	cmd := terminal.NewCommand(
		"/",
		fmt.Sprintf("/usr/bin/docker exec -t %s /bin/bash -c '"+strings.Join(compositeCommand, "; ")+"'", containerID),
	)
	stream, err := cmd.Run(ctx)
	if err != nil {
		return fmt.Errorf("error running command: %v", err)
	}

	if err := stream.Close(); err != nil {
		return fmt.Errorf("error closing stream: %v", err)
	}

	time.Sleep(1 * time.Second)

	return nil
}

func getSSHUser(codespace *api.Codespace) string {
	if codespace.RepositoryNWO == "github/github" {
		return "root"
	}
	return "codespace"
}
