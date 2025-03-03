/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gravitational/teleport"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/utils/retryutils"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/teleagent"
	"github.com/gravitational/teleport/lib/utils"
)

// TestSSH verifies "tsh ssh" command.
func TestSSH(t *testing.T) {
	lib.SetInsecureDevMode(true)
	defer lib.SetInsecureDevMode(false)

	s := newTestSuite(t,
		withRootConfigFunc(func(cfg *service.Config) {
			cfg.Version = defaults.TeleportConfigVersionV2
			cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
		}),
		withLeafCluster(),
		withLeafConfigFunc(func(cfg *service.Config) {
			cfg.Version = defaults.TeleportConfigVersionV2
			cfg.Proxy.SSHAddr.Addr = localListenerAddr()
		}),
	)

	tests := []struct {
		name string
		fn   func(t *testing.T, s *suite)
	}{
		{"ssh root cluster access", testRootClusterSSHAccess},
		{"ssh leaf cluster access", testLeafClusterSSHAccess},
		{"ssh jump host access", testJumpHostSSHAccess},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, s)
		})
	}
}

func testRootClusterSSHAccess(t *testing.T, s *suite) {
	tshHome := mustLogin(t, s)
	err := Run(context.Background(), []string{
		"ssh",
		s.root.Config.Hostname,
		"echo", "hello",
	}, setHomePath(tshHome))
	require.NoError(t, err)

	identityFile := mustLoginIdentity(t, s)
	err = Run(context.Background(), []string{
		"--proxy", s.root.Config.Proxy.WebAddr.String(),
		"--insecure",
		"-i", identityFile,
		"ssh",
		"localhost",
		"echo", "hello",
	}, setIdentity(identityFile))
	require.NoError(t, err)
}

func testLeafClusterSSHAccess(t *testing.T, s *suite) {
	tshHome := mustLogin(t, s, s.leaf.Config.Auth.ClusterName.GetClusterName())
	require.Eventually(t, func() bool {
		err := Run(context.Background(), []string{
			"ssh",
			"--proxy", s.root.Config.Proxy.WebAddr.String(),
			s.leaf.Config.Hostname,
			"echo", "hello",
		}, setHomePath(tshHome))
		t.Logf("ssh to leaf failed %v", err)
		return err == nil
	}, 5*time.Second, time.Second)

	identityFile := mustLoginIdentity(t, s)
	err := Run(context.Background(), []string{
		"--proxy", s.root.Config.Proxy.WebAddr.String(),
		"--insecure",
		"-i", identityFile,
		"ssh",
		"--cluster", s.leaf.Config.Auth.ClusterName.GetClusterName(),
		s.leaf.Config.Hostname,
		"echo", "hello",
	}, setIdentity(identityFile))
	require.NoError(t, err)
}

func testJumpHostSSHAccess(t *testing.T, s *suite) {
	// login to root
	tshHome := mustLogin(t, s, s.root.Config.Auth.ClusterName.GetClusterName())

	// Switch to leaf cluster
	err := Run(context.Background(), []string{
		"login",
		"--insecure",
		s.leaf.Config.Auth.ClusterName.GetClusterName(),
	}, setMockSSOLogin(t, s), setHomePath(tshHome))
	require.NoError(t, err)

	// Connect to leaf node though jump host set to leaf proxy SSH port.
	err = Run(context.Background(), []string{
		"ssh",
		"--insecure",
		"-J", s.leaf.Config.Proxy.SSHAddr.Addr,
		s.leaf.Config.Hostname,
		"echo", "hello",
	}, setMockSSOLogin(t, s), setHomePath(tshHome))
	require.NoError(t, err)

	t.Run("root cluster online", func(t *testing.T) {
		// Connect to leaf node though jump host set to proxy web port where TLS Routing is enabled.
		err = Run(context.Background(), []string{
			"ssh",
			"--insecure",
			"-J", s.leaf.Config.Proxy.WebAddr.Addr,
			s.leaf.Config.Hostname,
			"echo", "hello",
		}, setMockSSOLogin(t, s), setHomePath(tshHome))
		require.NoError(t, err)
	})

	t.Run("root cluster offline", func(t *testing.T) {
		// Terminate root cluster.
		err = s.root.Close()
		require.NoError(t, err)

		// Check JumpHost flow when root cluster is offline.
		err = Run(context.Background(), []string{
			"ssh",
			"--insecure",
			"-J", s.leaf.Config.Proxy.WebAddr.Addr,
			s.leaf.Config.Hostname,
			"echo", "hello",
		}, setMockSSOLogin(t, s), setHomePath(tshHome))
		require.NoError(t, err)
	})
}

// TestProxySSH verifies "tsh proxy ssh" functionality
func TestProxySSH(t *testing.T) {
	createAgent(t)

	tests := []struct {
		name string
		opts []testSuiteOptionFunc
	}{
		{
			name: "TLS routing enabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
				}),
			},
		},
		{
			name: "TLS routing disabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Separate)
				}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSuite(t, tc.opts...)

			proxyRequest := fmt.Sprintf("%s.%s:%d",
				s.root.Config.Proxy.SSHAddr.Host(),
				s.root.Config.Auth.ClusterName.GetClusterName(),
				s.root.Config.SSH.Addr.Port(defaults.SSHServerListenPort))

			runProxySSH := func(proxyRequest string, opts ...cliOption) error {
				return Run(context.Background(), []string{
					"--insecure",
					"--proxy", s.root.Config.Proxy.WebAddr.Addr,
					"proxy", "ssh", proxyRequest,
				}, opts...)
			}

			t.Run("login", func(t *testing.T) {
				t.Parallel()

				// Should fail without login
				err := runProxySSH(proxyRequest, setHomePath(t.TempDir()))
				require.Error(t, err)

				// Should succeed with login
				err = runProxySSH(proxyRequest, setHomePath(mustLogin(t, s)))
				require.NoError(t, err)
			})

			t.Run("re-login", func(t *testing.T) {
				t.Parallel()

				err := runProxySSH(proxyRequest, setHomePath(mustLogin(t, s)), setMockSSOLogin(t, s))
				require.NoError(t, err)
			})

			t.Run("identity file", func(t *testing.T) {
				t.Parallel()

				err := runProxySSH(proxyRequest, setIdentity(mustLoginIdentity(t, s)))
				require.NoError(t, err)
			})

			t.Run("invalid node login", func(t *testing.T) {
				t.Parallel()

				invalidLoginRequest := fmt.Sprintf("%s@%s", "invalidUser", proxyRequest)
				err := runProxySSH(invalidLoginRequest, setHomePath(mustLogin(t, s)), setMockSSOLogin(t, s))
				require.Error(t, err)
				require.True(t, utils.IsHandshakeFailedError(err), "expected handshake error, got %v", err)
			})
		})

	}
}

// TestTSHProxyTemplate verifies connecting with OpenSSH client through the
// local proxy started with "tsh proxy ssh -J" using proxy templates.
func TestTSHProxyTemplate(t *testing.T) {
	_, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("Skipping test, no ssh binary found.")
	}

	lib.SetInsecureDevMode(true)
	defer lib.SetInsecureDevMode(false)

	tshPath, err := os.Executable()
	require.NoError(t, err)

	s := newTestSuite(t)
	tshHome := mustLoginSetEnv(t, s)

	// Create proxy template configuration.
	tshConfigFile := filepath.Join(tshHome, tshConfigPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(tshConfigFile), 0777))
	require.NoError(t, os.WriteFile(tshConfigFile, []byte(fmt.Sprintf(`
proxy_templates:
- template: '^(\w+)\.(root):(.+)$'
  proxy: "%v"
  host: "$1:$3"
`, s.root.Config.Proxy.WebAddr.Addr)), 0644))

	// Create SSH config.
	sshConfigFile := filepath.Join(tshHome, "sshconfig")
	os.WriteFile(sshConfigFile, []byte(fmt.Sprintf(`
Host *
  HostName %%h
  StrictHostKeyChecking no
  ProxyCommand %v -d --insecure proxy ssh -J {{proxy}} %%r@%%h:%%p
`, tshPath)), 0644)

	// Connect to "localnode" with OpenSSH.
	mustRunOpenSSHCommand(t, sshConfigFile, "localnode.root",
		s.root.Config.SSH.Addr.Port(defaults.SSHServerListenPort), "echo", "hello")
}

// TestTSHConfigConnectWithOpenSSHClient tests OpenSSH configuration generated by tsh config command and
// connects to ssh node using native OpenSSH client with different session recording  modes and proxy listener modes.
func TestTSHConfigConnectWithOpenSSHClient(t *testing.T) {
	// Only run this test if we have access to the external SSH binary.
	_, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("Skipping no external SSH binary found.")
	}

	lib.SetInsecureDevMode(true)
	defer lib.SetInsecureDevMode(false)

	tests := []struct {
		name string
		opts []testSuiteOptionFunc
	}{
		{
			name: "node recording mode with TLS routing enabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.SessionRecordingConfig.SetMode(types.RecordAtNode)
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
				}),
			},
		},
		{
			name: "proxy recording mode with TLS routing enabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.SessionRecordingConfig.SetMode(types.RecordAtProxy)
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
				}),
			},
		},
		{
			name: "node recording mode with TLS routing disabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.SessionRecordingConfig.SetMode(types.RecordAtNode)
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Separate)
				}),
			},
		},
		{
			name: "proxy recording mode with TLS routing disabled",
			opts: []testSuiteOptionFunc{
				withRootConfigFunc(func(cfg *service.Config) {
					cfg.Auth.SessionRecordingConfig.SetMode(types.RecordAtProxy)
					cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Separate)
				}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSuite(t, tc.opts...)

			// Login to the Teleport proxy.
			mustLoginSetEnv(t, s)

			// Get SSH config file generated by the 'tsh config' command.
			sshConfigFile := mustGetOpenSSHConfigFile(t)

			sshConn := fmt.Sprintf("%s.%s", s.root.Config.Hostname, s.root.Config.Auth.ClusterName.GetClusterName())
			nodePort := s.root.Config.SSH.Addr.Port(defaults.SSHServerListenPort)
			bashCmd := []string{"echo", "hello"}

			// Run ssh command using the OpenSSH client.
			mustRunOpenSSHCommand(t, sshConfigFile, sshConn, nodePort, bashCmd...)

			// Try to run ssh command using the OpenSSH client with invalid node login username.
			// Command should fail because nodeLogin 'invalidUser' is not in valid principals.
			sshConn = fmt.Sprintf("invalidUser@%s", sshConn)
			mustFailToRunOpenSSHCommand(t, sshConfigFile, sshConn, nodePort, bashCmd...)

			// Check if failed login attempt event has proper nodeLogin.
			mustFindFailedNodeLoginAttempt(t, s, "invalidUser")
		})
	}
}

func TestEnvVarCommand(t *testing.T) {
	tests := []struct {
		inputFormat  string
		inputKey     string
		inputValue   string
		expectOutput string
		expectError  bool
	}{
		{
			inputFormat:  envVarFormatText,
			inputKey:     "key",
			inputValue:   "value",
			expectOutput: "key=value",
		},
		{
			inputFormat:  envVarFormatUnix,
			inputKey:     "key",
			inputValue:   "value",
			expectOutput: "export key=value",
		},
		{
			inputFormat:  envVarFormatWindowsCommandPrompt,
			inputKey:     "key",
			inputValue:   "value",
			expectOutput: "set key=value",
		},
		{
			inputFormat:  envVarFormatWindowsPowershell,
			inputKey:     "key",
			inputValue:   "value",
			expectOutput: "$Env:key=\"value\"",
		},
		{
			inputFormat: "unknown",
			inputKey:    "key",
			inputValue:  "value",
			expectError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.inputFormat, func(t *testing.T) {
			actualOutput, err := envVarCommand(test.inputFormat, test.inputKey, test.inputValue)
			if test.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.expectOutput, actualOutput)
			}
		})
	}
}

func createAgent(t *testing.T) string {
	t.Helper()

	user, err := user.Current()
	require.NoError(t, err)

	sockDir := "test"
	sockName := "agent.sock"

	keyring := agent.NewKeyring()
	teleAgent := teleagent.NewServer(func() (teleagent.Agent, error) {
		return teleagent.NopCloser(keyring), nil
	})

	// Start the SSH agent.
	err = teleAgent.ListenUnixSocket(sockDir, sockName, user)
	require.NoError(t, err)
	go teleAgent.Serve()
	t.Cleanup(func() {
		teleAgent.Close()
	})

	t.Setenv(teleport.SSHAuthSock, teleAgent.Path)

	return teleAgent.Path
}

func setMockSSOLogin(t *testing.T, s *suite) cliOption {
	return func(cf *CLIConf) error {
		cf.mockSSOLogin = mockSSOLogin(t, s.root.GetAuthServer(), s.user)
		cf.AuthConnector = s.connector.GetName()
		return nil
	}
}

func mustLogin(t *testing.T, s *suite, args ...string) string {
	tshHome := t.TempDir()
	args = append([]string{
		"login",
		"--insecure",
		"--debug",
		"--proxy", s.root.Config.Proxy.WebAddr.String(),
	}, args...)
	err := Run(context.Background(), args, setMockSSOLogin(t, s), setHomePath(tshHome))
	require.NoError(t, err)
	return tshHome
}

// login with new temp tshHome and set it in Env. This is useful
// when running "ssh" commands with a tsh "ProxyCommand".
func mustLoginSetEnv(t *testing.T, s *suite, args ...string) string {
	tshHome := t.TempDir()
	t.Setenv(types.HomeEnvVar, tshHome)

	args = append([]string{
		"login",
		"--insecure",
		"--debug",
		"--proxy", s.root.Config.Proxy.WebAddr.String(),
	}, args...)
	err := Run(context.Background(), args, setMockSSOLogin(t, s), setHomePath(tshHome))
	require.NoError(t, err)
	return tshHome
}

func mustLoginIdentity(t *testing.T, s *suite, opts ...cliOption) string {
	identityFile := path.Join(t.TempDir(), "identity.pem")
	mustLogin(t, s, "--out", identityFile)
	return identityFile
}

func mustGetOpenSSHConfigFile(t *testing.T) string {
	var buff bytes.Buffer
	err := Run(context.Background(), []string{
		"config",
	}, func(cf *CLIConf) error {
		cf.overrideStdout = &buff
		return nil
	})
	require.NoError(t, err)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "ssh_config")
	err = os.WriteFile(configPath, buff.Bytes(), 0600)
	require.NoError(t, err)

	return configPath
}

func runOpenSSHCommand(t *testing.T, configFile string, sshConnString string, port int, args ...string) error {
	ss := []string{
		"-F", configFile,
		"-y",
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=no",
		sshConnString,
	}

	ss = append(ss, args...)

	sshPath, err := exec.LookPath("ssh")
	require.NoError(t, err)

	cmd := exec.Command(sshPath, ss...)
	cmd.Env = []string{
		fmt.Sprintf("%s=1", tshBinMainTestEnv),
		fmt.Sprintf("SSH_AUTH_SOCK=%s", createAgent(t)),
		fmt.Sprintf("PATH=%s", filepath.Dir(sshPath)),
		fmt.Sprintf("%s=%s", types.HomeEnvVar, os.Getenv(types.HomeEnvVar)),
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout
	return trace.Wrap(cmd.Run())
}

func mustRunOpenSSHCommand(t *testing.T, configFile string, sshConnString string, port int, args ...string) {
	err := retryutils.RetryStaticFor(time.Second*10, time.Millisecond*500, func() error {
		err := runOpenSSHCommand(t, configFile, sshConnString, port, args...)
		return trace.Wrap(err)
	})
	require.NoError(t, err)
}

func mustFailToRunOpenSSHCommand(t *testing.T, configFile string, sshConnString string, port int, args ...string) {
	err := runOpenSSHCommand(t, configFile, sshConnString, port, args...)
	require.Error(t, err)
}

func mustSearchEvents(t *testing.T, auth *auth.Server) []apievents.AuditEvent {
	now := time.Now()
	events, _, err := auth.SearchEvents(
		now.Add(-time.Hour),
		now.Add(time.Hour),
		apidefaults.Namespace,
		nil,
		0,
		types.EventOrderDescending,
		"")

	require.NoError(t, err)
	return events
}

func mustFindFailedNodeLoginAttempt(t *testing.T, s *suite, nodeLogin string) {
	av := mustSearchEvents(t, s.root.GetAuthServer())
	for _, e := range av {
		if e.GetCode() == events.AuthAttemptFailureCode {
			require.Equal(t, e.(*apievents.AuthAttempt).Login, nodeLogin)
			return
		}
	}
	t.Errorf("failed to find AuthAttemptFailureCode event (0/%d events matched)", len(av))
}
