package executor

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorcon/rcon"
	"github.com/crasssr/rcon-cli/internal/config"
	"github.com/crasssr/rcon-cli/internal/logger"
	"github.com/gorcon/telnet"
	"github.com/gorcon/websocket"
	"github.com/urfave/cli/v2"
)

// CommandQuit is the command for exit from Interactive mode.
const CommandQuit = ":q"

// CommandsResponseSeparator is symbols that is written between responses of
// several commands if more than one command was called.
const CommandsResponseSeparator = "--------"

// Errors.
var (
	// ErrEmptyAddress is returned when executed command without setting address
	// in single mode.
	ErrEmptyAddress = errors.New("address is not set: to set address add -a host:port")

	// ErrEmptyPassword is returned when executed command without setting password
	// in single mode.
	ErrEmptyPassword = errors.New("password is not set: to set password add -p password")

	// ErrCommandEmpty is returned when executed command length equal 0.
	ErrCommandEmpty = errors.New("command is not set")
)

// ExecuteCloser is the interface that groups Execute and Close methods.
type ExecuteCloser interface {
	Execute(command string) (string, error)
	Close() error
}

// Executor is a cli commands execute wrapper.
type Executor struct {
	version string
	r       io.Reader
	w       io.Writer
	app     *cli.App

	client ExecuteCloser
}

// Apply or remove color codes text
func processColorCodes(text string, stripColors bool) string {
    // Map of Minecraft color codes to ANSI escape codes.
    colorMap := map[rune]string{
        '0': "\033[30m", // Black
        '1': "\033[34m", // Dark Blue
        '2': "\033[32m", // Dark Green
        '3': "\033[36m", // Dark Aqua
        '4': "\033[31m", // Dark Red
        '5': "\033[35m", // Dark Purple
        '6': "\033[33m", // Gold
        '7': "\033[37m", // Gray
        '8': "\033[90m", // Dark Gray
        '9': "\033[94m", // Blue
        'a': "\033[92m", // Green
        'b': "\033[96m", // Aqua
        'c': "\033[91m", // Red
        'd': "\033[95m", // Light Purple
        'e': "\033[93m", // Yellow
        'f': "\033[97m", // White
        'r': "\033[0m",  // Reset
        // Add more as needed
    }
	
	if stripColors {
        // Remove color codes by stripping § and following character.
        return strings.Map(func(r rune) rune {
            if r == '§' {
                return -1
            }
            return r
        }, text)
    } else {
        // Apply ANSI color codes.
        var result strings.Builder
        skip := false

        for i, r := range text {
            if skip {
                skip = false
                continue
            }
            if r == '§' && i+1 < len(text) {
                color, ok := colorMap[rune(text[i+1])]
                if ok {
                    result.WriteString(color)
                    skip = true
                    continue
                }
            }
            result.WriteRune(r)
        }

        // Ensure reset at the end.
        result.WriteString("\033[0m")
        return result.String()
    }
}

// NewExecutor creates a new Executor.
func NewExecutor(r io.Reader, w io.Writer, version string) *Executor {
	return &Executor{
		version: version,
		r:       r,
		w:       w,
	}
}

// Run is the entry point to the cli app.
func (executor *Executor) Run(arguments []string) error {
	executor.init()

	if err := executor.app.Run(arguments); err != nil && !errors.Is(err, flag.ErrHelp) {
		return fmt.Errorf("cli: %w", err)
	}

	return nil
}

// NewSession parses os args and config file for connection details to
// a remote server. If the address and password flags were received the
// configuration file is ignored.
func (executor *Executor) NewSession(c *cli.Context) (*config.Session, error) {
	ses := config.Session{
		Address:    c.String("address"),
		Password:   c.String("password"),
		Type:       c.String("type"),
		Log:        c.String("log"),
		SkipErrors: c.Bool("skip"),
		Timeout:    c.Duration("timeout"),
		Variables:  c.Bool("variables"),
	}

	if ses.Address != "" && ses.Password != "" {
		return &ses, nil
	}

	cfg, err := config.NewConfig(c.String("config"))
	if err != nil {
		return &ses, fmt.Errorf("config: %w", err)
	}

	env := c.String("env")
	if env == "" {
		env = config.DefaultConfigEnv
	}

	// Get variables from config environment if flags are not defined.
	if ses.Address == "" {
		ses.Address = (*cfg)[env].Address
	}

	if ses.Password == "" {
		ses.Password = (*cfg)[env].Password
	}

	if ses.Log == "" {
		ses.Log = (*cfg)[env].Log
	}

	if ses.Type == "" {
		ses.Type = (*cfg)[env].Type
	}

	return &ses, nil
}

// Dial sends auth request for remote server. Returns en error if
// address or password is incorrect.
func (executor *Executor) Dial(ses *config.Session) error {
	var err error

	if executor.client == nil {
		switch ses.Type {
		case config.ProtocolTELNET:
			executor.client, err = telnet.Dial(ses.Address, ses.Password, telnet.SetDialTimeout(ses.Timeout))
		case config.ProtocolWebRCON:
			executor.client, err = websocket.Dial(
				ses.Address, ses.Password, websocket.SetDialTimeout(ses.Timeout), websocket.SetDeadline(ses.Timeout))
		default:
			executor.client, err = rcon.Dial(
				ses.Address, ses.Password, rcon.SetDialTimeout(ses.Timeout), rcon.SetDeadline(ses.Timeout))
		}
	}

	if err != nil {
		executor.client = nil

		return fmt.Errorf("auth: %w", err)
	}

	return nil
}

// Execute sends commands to Execute to the remote server and prints the response.
func (executor *Executor) Execute(w io.Writer, ses *config.Session, commands ...string) error {
	if len(commands) == 0 {
		return ErrCommandEmpty
	}

	// TODO: Check keep alive connection to web rcon.
	if ses.Type == config.ProtocolWebRCON {
		defer func() {
			if executor.client != nil {
				_ = executor.client.Close()
				executor.client = nil
			}
		}()
	}

	if err := executor.Dial(ses); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	for i, command := range commands {
		if err := executor.execute(w, ses, command); err != nil {
			return err
		}

		if i+1 != len(commands) {
			_, _ = fmt.Fprintln(w, CommandsResponseSeparator)
		}
	}

	return nil
}

// Interactive reads stdin, parses commands, executes them on remote server
// and prints the responses.
func (executor *Executor) Interactive(r io.Reader, w io.Writer, ses *config.Session) error {
	if ses.Address == "" {
		_, _ = fmt.Fprint(w, "Enter remote host and port [ip:port]: ")
		_, _ = fmt.Fscanln(r, &ses.Address)
	}

	if ses.Password == "" {
		_, _ = fmt.Fprint(w, "Enter password: ")
		_, _ = fmt.Fscanln(r, &ses.Password)
	}

	if ses.Type == "" {
		_, _ = fmt.Fprint(w, "Enter protocol type (empty for rcon): ")
		_, _ = fmt.Fscanln(r, &ses.Type)
	}

	switch ses.Type {
	case config.ProtocolTELNET:
		return telnet.DialInteractive(r, w, ses.Address, ses.Password)
	case "", config.ProtocolRCON, config.ProtocolWebRCON:
		if err := executor.Dial(ses); err != nil {
			return err
		}

		_, _ = fmt.Fprintf(w, "Waiting commands for %s (or type %s to exit)\n> ", ses.Address, CommandQuit)

		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			command := scanner.Text()
			if command != "" {
				if command == CommandQuit {
					break
				}

				if err := executor.Execute(w, ses, command); err != nil {
					return err
				}
			}

			_, _ = fmt.Fprint(w, "> ")
		}
	default:
		_, _ = fmt.Fprintf(w, "Unsupported protocol type (%q). Allowed %q, %q and %q protocols\n",
			ses.Type, config.ProtocolRCON, config.ProtocolWebRCON, config.ProtocolTELNET)
	}

	return nil
}

// Close closes connection to remote server.
func (executor *Executor) Close() error {
	if executor.client != nil {
		return executor.client.Close()
	}

	return nil
}

// init creates a new cli Application.
func (executor *Executor) init() {
	app := cli.NewApp()
	app.Usage = "CLI for executing queries on a remote server"
	app.Description = "Can be run in two modes - in the mode of a single query and in terminal mode of reading the " +
		"input stream. \n\n" + "To run single mode type commands after options flags. Example: \n" +
		filepath.Base(os.Args[0]) + " -a 127.0.0.1:16260 -p password command1 command2 \n\n" +
		"To run terminal mode just do not specify commands to execute. Example: \n" +
		filepath.Base(os.Args[0]) + " -a 127.0.0.1:16260 -p password"
	app.Version = executor.version
	app.Copyright = "Copyright (c) 2022 Pavel Korotkiy (outdead)"
	app.HideHelpCommand = true
	app.Flags = executor.getFlags()
	app.Action = executor.action

	executor.app = app
}

// getFlags returns CLI flags to parse.
func (executor *Executor) getFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "address",
			Aliases: []string{"a"},
			Usage:   "Set host and port to remote server. Example 127.0.0.1:16260",
		},
		&cli.StringFlag{
			Name:    "password",
			Aliases: []string{"p"},
			Usage:   "Set password to remote server",
		},
		&cli.StringFlag{
			Name:    "type",
			Aliases: []string{"t"},
			Usage:   "Specify type of connection",
			Value:   config.DefaultProtocol,
		},
		&cli.StringFlag{
			Name:    "log",
			Aliases: []string{"l"},
			Usage:   "Path to the log file. If not specified it is taken from the config",
		},
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "Path to the configuration file",
			Value:   config.DefaultConfigName,
		},
		&cli.StringFlag{
			Name:    "env",
			Aliases: []string{"e"},
			Usage:   "Config environment with server credentials",
			Value:   config.DefaultConfigEnv,
		},
		&cli.BoolFlag{
			Name:    "skip",
			Aliases: []string{"s"},
			Usage:   "Skip errors and run next command",
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Aliases: []string{"T"},
			Usage:   "Set dial and execute timeout",
			Value:   config.DefaultTimeout,
		},
		&cli.BoolFlag{
			Name:    "variables",
			Aliases: []string{"V"},
			Usage:   "Print stored variables and exit",
			Value:   false,
		},
	}
}

// action executes when no subcommands are specified.
func (executor *Executor) action(c *cli.Context) error {
	ses, err := executor.NewSession(c)
	if err != nil {
		return err
	}

	if ses.Variables {
		executor.printVariables(ses, c)

		return nil
	}

	commands := c.Args().Slice()
	if len(commands) == 0 {
		return executor.Interactive(executor.r, executor.w, ses)
	}

	if ses.Address == "" {
		return ErrEmptyAddress
	}

	if ses.Password == "" {
		return ErrEmptyPassword
	}

	return executor.Execute(executor.w, ses, commands...)
}

// execute sends command to Execute to the remote server and prints the response.
func (executor *Executor) execute(w io.Writer, ses *config.Session, command string) error {
	if command == "" {
		return ErrCommandEmpty
	}

	var result string
	var err error

	result, err = executor.client.Execute(command)
	if result != "" {
		result = strings.TrimSpace(result)

		// Minecraft code here
		stripColors := false // Set this based on your needs or configuration
		result = processColorCodes(result, stripColors)
		
		_, _ = fmt.Fprintln(w, result)
	}

	if err != nil {
		if ses.SkipErrors {
			_, _ = fmt.Fprintln(w, fmt.Errorf("execute: %w", err))
		} else {
			return fmt.Errorf("execute: %w", err)
		}
	}

	if err = logger.Write(ses.Log, ses.Address, command, result); err != nil {
		_, _ = fmt.Fprintln(w, fmt.Errorf("log: %w", err))
	}

	return nil
}

func (executor *Executor) printVariables(ses *config.Session, c *cli.Context) {
	_, _ = fmt.Fprint(executor.w, "Got Print Variables param.\n")
	_ = ses.Print(executor.w)

	_, _ = fmt.Fprint(executor.w, "\nPrint other variables:\n")
	_, _ = fmt.Fprintf(executor.w, "Path to config file (if used): %s\n", c.String("config"))
	_, _ = fmt.Fprintf(executor.w, "Cofig environment: %s\n", c.String("env"))
}
