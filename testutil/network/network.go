package network

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cometbft/cometbft/v2/node"
	cmtclient "github.com/cometbft/cometbft/v2/rpc/client"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"cosmossdk.io/depinject"
	"cosmossdk.io/log"
	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/math/unsafe"
	pruningtypes "cosmossdk.io/store/pruning/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/server/api"
	srvconfig "github.com/cosmos/cosmos-sdk/server/config"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	"github.com/cosmos/cosmos-sdk/testutil"
	"github.com/cosmos/cosmos-sdk/testutil/configurator"
	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	_ "github.com/cosmos/cosmos-sdk/x/auth"           // import auth as a blank
	_ "github.com/cosmos/cosmos-sdk/x/auth/tx/config" // import auth tx config as a blank
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	_ "github.com/cosmos/cosmos-sdk/x/bank" // import bank as a blank
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	_ "github.com/cosmos/cosmos-sdk/x/consensus" // import consensus as a blank
	"github.com/cosmos/cosmos-sdk/x/genutil"
	_ "github.com/cosmos/cosmos-sdk/x/params"  // import params as a blank
	_ "github.com/cosmos/cosmos-sdk/x/staking" // import staking as a blank
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// package-wide network lock to only allow one test network at a time
var (
	lock     = new(sync.Mutex)
	portPool = make(chan string, 200)
)

func init() {
	closeFns := []func() error{}
	for range 200 {
		_, port, closeFn, err := FreeTCPAddr()
		if err != nil {
			panic(err)
		}

		portPool <- port
		closeFns = append(closeFns, closeFn)
	}

	for _, closeFn := range closeFns {
		err := closeFn()
		if err != nil {
			panic(err)
		}
	}
}

type (
	// AppConstructor defines a function which accepts a network configuration and
	// creates an ABCI Application to provide to CometBFT.
	AppConstructor     = func(val ValidatorI) servertypes.Application
	TestFixtureFactory = func() TestFixture
)

type TestFixture struct {
	AppConstructor AppConstructor
	GenesisState   map[string]json.RawMessage
	EncodingConfig moduletestutil.TestEncodingConfig
}

// Config defines the necessary configuration used to bootstrap and start an
// in-process local testing network.
type Config struct {
	Codec             codec.Codec
	LegacyAmino       *codec.LegacyAmino // TODO: Remove!
	InterfaceRegistry codectypes.InterfaceRegistry

	TxConfig         client.TxConfig
	AccountRetriever client.AccountRetriever
	AppConstructor   AppConstructor             // the ABCI application constructor
	GenesisState     map[string]json.RawMessage // custom genesis state to provide
	TimeoutCommit    time.Duration              // the consensus commitment timeout
	ChainID          string                     // the network chain-id
	NumValidators    int                        // the total number of validators to create and bond
	Mnemonics        []string                   // custom user-provided validator operator mnemonics
	BondDenom        string                     // the staking bond denomination
	MinGasPrices     string                     // the minimum gas prices each validator will accept
	AccountTokens    sdkmath.Int                // the amount of unique validator tokens (e.g. 1000node0)
	StakingTokens    sdkmath.Int                // the amount of tokens each validator has available to stake
	BondedTokens     sdkmath.Int                // the amount of tokens each validator stakes
	PruningStrategy  string                     // the pruning strategy each validator will have
	EnableLogging    bool                       // enable logging to STDOUT
	CleanupDir       bool                       // remove base temporary directory during cleanup
	SigningAlgo      string                     // signing algorithm for keys
	KeyringOptions   []keyring.Option           // keyring configuration options
	RPCAddress       string                     // RPC listen address (including port)
	APIAddress       string                     // REST API listen address (including port)
	GRPCAddress      string                     // GRPC server listen address (including port)
	PrintMnemonic    bool                       // print the mnemonic of first validator as log output for testing
}

// DefaultConfig returns a sane default configuration suitable for nearly all
// testing requirements.
func DefaultConfig(factory TestFixtureFactory) Config {
	fixture := factory()

	return Config{
		Codec:             fixture.EncodingConfig.Codec,
		TxConfig:          fixture.EncodingConfig.TxConfig,
		LegacyAmino:       fixture.EncodingConfig.Amino,
		InterfaceRegistry: fixture.EncodingConfig.InterfaceRegistry,
		AccountRetriever:  authtypes.AccountRetriever{},
		AppConstructor:    fixture.AppConstructor,
		GenesisState:      fixture.GenesisState,
		TimeoutCommit:     2 * time.Second,
		ChainID:           "chain-" + unsafe.Str(6),
		NumValidators:     4,
		BondDenom:         sdk.DefaultBondDenom,
		MinGasPrices:      fmt.Sprintf("0.000006%s", sdk.DefaultBondDenom),
		AccountTokens:     sdk.TokensFromConsensusPower(1000, sdk.DefaultPowerReduction),
		StakingTokens:     sdk.TokensFromConsensusPower(500, sdk.DefaultPowerReduction),
		BondedTokens:      sdk.TokensFromConsensusPower(100, sdk.DefaultPowerReduction),
		PruningStrategy:   pruningtypes.PruningOptionNothing,
		CleanupDir:        true,
		SigningAlgo:       string(hd.Secp256k1Type),
		KeyringOptions:    []keyring.Option{},
		PrintMnemonic:     false,
	}
}

// MinimumAppConfig defines the minimum of modules required for a call to New to succeed
func MinimumAppConfig() depinject.Config {
	return configurator.NewAppConfig(
		configurator.AuthModule(),
		configurator.ParamsModule(),
		configurator.BankModule(),
		configurator.GenutilModule(),
		configurator.StakingModule(),
		configurator.ConsensusModule(),
		configurator.TxModule(),
	)
}

// DefaultConfigWithAppConfig returns a network configuration constructed using
// the provided app config. It sets an infinite gas limit on queries by passing zero
// as the query gas limit (i.e. disabling gas metering for queries). This config is
// suitable for testing scenarios where queries are allowed to consume unbounded gas.
//
// It is equivalent to calling DefaultConfigWithAppConfigWithQueryGasLimit(appConfig, 0).
func DefaultConfigWithAppConfig(appConfig depinject.Config) (Config, error) {
	return DefaultConfigWithAppConfigWithQueryGasLimit(appConfig, 0)
}

// DefaultConfigWithAppConfigWithQueryGasLimit returns a network configuration constructed
// using the provided app config and the specified query gas limit.
func DefaultConfigWithAppConfigWithQueryGasLimit(appConfig depinject.Config, queryGasLimit uint64) (Config, error) {
	var (
		appBuilder        *runtime.AppBuilder
		txConfig          client.TxConfig
		legacyAmino       *codec.LegacyAmino
		cdc               codec.Codec
		interfaceRegistry codectypes.InterfaceRegistry
	)

	if err := depinject.Inject(
		depinject.Configs(
			appConfig,
			depinject.Supply(log.NewNopLogger()),
		),
		&appBuilder,
		&txConfig,
		&cdc,
		&legacyAmino,
		&interfaceRegistry,
	); err != nil {
		return Config{}, err
	}

	cfg := DefaultConfig(func() TestFixture {
		return TestFixture{}
	})
	cfg.Codec = cdc
	cfg.TxConfig = txConfig
	cfg.LegacyAmino = legacyAmino
	cfg.InterfaceRegistry = interfaceRegistry
	cfg.GenesisState = appBuilder.DefaultGenesis()
	cfg.AppConstructor = func(val ValidatorI) servertypes.Application {
		// we build a unique app instance for every validator here
		var appBuilder *runtime.AppBuilder
		if err := depinject.Inject(
			depinject.Configs(
				appConfig,
				depinject.Supply(val.GetCtx().Logger),
			),
			&appBuilder); err != nil {
			panic(err)
		}
		app := appBuilder.Build(
			dbm.NewMemDB(),
			nil,
			baseapp.SetPruning(pruningtypes.NewPruningOptionsFromString(val.GetAppConfig().Pruning)),
			baseapp.SetMinGasPrices(val.GetAppConfig().MinGasPrices),
			baseapp.SetChainID(cfg.ChainID),
			baseapp.SetQueryGasLimit(queryGasLimit),
		)

		testdata.RegisterQueryServer(app.GRPCQueryRouter(), testdata.QueryImpl{})

		if err := app.Load(true); err != nil {
			panic(err)
		}

		return app
	}

	return cfg, nil
}

type (
	// Network defines a local in-process testing network using SimApp. It can be
	// configured to start any number of validators, each with its own RPC and API
	// clients. Typically, this test network would be used in client and integration
	// testing where user input is expected.
	//
	// Note, due to CometBFT constraints in regards to RPC functionality, there
	// may only be one test network running at a time. Thus, any caller must be
	// sure to Cleanup after testing is finished in order to allow other tests
	// to create networks. In addition, only the first validator will have a valid
	// RPC and API server/client.
	Network struct {
		Logger     Logger
		BaseDir    string
		Validators []*Validator

		Config Config
	}

	// Validator defines an in-process CometBFT validator node. Through this object,
	// a client can make RPC and API calls and interact with any client command
	// or handler.
	Validator struct {
		AppConfig  *srvconfig.Config
		ClientCtx  client.Context
		Ctx        *server.Context
		Dir        string
		NodeID     string
		PubKey     cryptotypes.PubKey
		Moniker    string
		APIAddress string
		RPCAddress string
		P2PAddress string
		Address    sdk.AccAddress
		ValAddress sdk.ValAddress
		RPCClient  cmtclient.Client

		app      servertypes.Application
		tmNode   *node.Node
		api      *api.Server
		grpc     *grpc.Server
		grpcWeb  *http.Server
		errGroup *errgroup.Group
		cancelFn context.CancelFunc
	}

	// ValidatorI expose a validator's context and configuration
	ValidatorI interface {
		GetCtx() *server.Context
		GetAppConfig() *srvconfig.Config
	}

	// Logger is a network logger interface that exposes testnet-level Log() methods for an in-process testing network
	// This is not to be confused with logging that may happen at an individual node or validator level
	Logger interface {
		Log(args ...any)
		Logf(format string, args ...any)
	}
)

var (
	_ Logger     = (*testing.T)(nil)
	_ Logger     = (*CLILogger)(nil)
	_ ValidatorI = Validator{}
)

func (v Validator) GetCtx() *server.Context {
	return v.Ctx
}

func (v Validator) GetAppConfig() *srvconfig.Config {
	return v.AppConfig
}

// CLILogger wraps a cobra.Command and provides command logging methods.
type CLILogger struct {
	cmd *cobra.Command
}

// Log logs given args.
func (s CLILogger) Log(args ...any) {
	s.cmd.Println(args...)
}

// Logf logs given args according to a format specifier.
func (s CLILogger) Logf(format string, args ...any) {
	s.cmd.Printf(format, args...)
}

// NewCLILogger creates a new CLILogger.
func NewCLILogger(cmd *cobra.Command) CLILogger {
	return CLILogger{cmd}
}

// New creates a new Network for integration tests or in-process testnets run via the CLI
func New(l Logger, baseDir string, cfg Config) (*Network, error) {
	// only one caller/test can create and use a network at a time
	l.Log("acquiring test network lock")
	lock.Lock()

	network := &Network{
		Logger:     l,
		BaseDir:    baseDir,
		Validators: make([]*Validator, cfg.NumValidators),
		Config:     cfg,
	}

	l.Logf("preparing test network with chain-id \"%s\"\n", cfg.ChainID)

	monikers := make([]string, cfg.NumValidators)
	nodeIDs := make([]string, cfg.NumValidators)
	valPubKeys := make([]cryptotypes.PubKey, cfg.NumValidators)

	var (
		genAccounts []authtypes.GenesisAccount
		genBalances []banktypes.Balance
		genFiles    []string
	)

	buf := bufio.NewReader(os.Stdin)

	// generate private keys, node IDs, and initial transactions
	for i := range cfg.NumValidators {
		appCfg := srvconfig.DefaultConfig()
		appCfg.Pruning = cfg.PruningStrategy
		appCfg.MinGasPrices = cfg.MinGasPrices
		appCfg.API.Enable = true
		appCfg.API.Swagger = false
		appCfg.Telemetry.Enabled = false

		ctx := server.NewDefaultContext()
		cmtCfg := ctx.Config
		cmtCfg.Consensus.TimeoutCommit = cfg.TimeoutCommit // nolint: staticcheck // we are continuing to use this value for backwards compatibility

		// Only allow the first validator to expose an RPC, API and gRPC
		// server/client due to CometBFT in-process constraints.
		apiAddr := ""
		cmtCfg.RPC.ListenAddress = ""
		appCfg.GRPC.Enable = false
		appCfg.GRPCWeb.Enable = false
		apiListenAddr := ""
		if i == 0 {
			if cfg.APIAddress != "" {
				apiListenAddr = cfg.APIAddress
			} else {
				if len(portPool) == 0 {
					return nil, fmt.Errorf("failed to get port for API server")
				}
				port := <-portPool
				apiListenAddr = fmt.Sprintf("tcp://0.0.0.0:%s", port)
			}

			appCfg.API.Address = apiListenAddr
			apiURL, err := url.Parse(apiListenAddr)
			if err != nil {
				return nil, err
			}
			apiAddr = fmt.Sprintf("http://%s:%s", apiURL.Hostname(), apiURL.Port())

			if cfg.RPCAddress != "" {
				cmtCfg.RPC.ListenAddress = cfg.RPCAddress
			} else {
				if len(portPool) == 0 {
					return nil, fmt.Errorf("failed to get port for RPC server")
				}
				port := <-portPool
				cmtCfg.RPC.ListenAddress = fmt.Sprintf("tcp://0.0.0.0:%s", port)
			}

			if cfg.GRPCAddress != "" {
				appCfg.GRPC.Address = cfg.GRPCAddress
			} else {
				if len(portPool) == 0 {
					return nil, fmt.Errorf("failed to get port for GRPC server")
				}
				port := <-portPool
				appCfg.GRPC.Address = fmt.Sprintf("0.0.0.0:%s", port)
			}
			appCfg.GRPC.Enable = true
			appCfg.GRPCWeb.Enable = true
		}

		logger := log.NewNopLogger()
		if cfg.EnableLogging {
			logger = log.NewLogger(os.Stdout) // TODO(mr): enable selection of log destination.
		}

		ctx.Logger = logger

		nodeDirName := fmt.Sprintf("node%d", i)
		nodeDir := filepath.Join(network.BaseDir, nodeDirName, "simd")
		clientDir := filepath.Join(network.BaseDir, nodeDirName, "simcli")
		gentxsDir := filepath.Join(network.BaseDir, "gentxs")

		err := os.MkdirAll(filepath.Join(nodeDir, "config"), 0o755)
		if err != nil {
			return nil, err
		}

		err = os.MkdirAll(clientDir, 0o755)
		if err != nil {
			return nil, err
		}

		cmtCfg.SetRoot(nodeDir)
		cmtCfg.Moniker = nodeDirName
		monikers[i] = nodeDirName

		if len(portPool) == 0 {
			return nil, fmt.Errorf("failed to get port for Proxy server")
		}
		port := <-portPool
		proxyAddr := fmt.Sprintf("tcp://0.0.0.0:%s", port)
		cmtCfg.ProxyApp = proxyAddr

		if len(portPool) == 0 {
			return nil, fmt.Errorf("failed to get port for Proxy server")
		}
		port = <-portPool
		p2pAddr := fmt.Sprintf("tcp://0.0.0.0:%s", port)
		cmtCfg.P2P.ListenAddress = p2pAddr
		cmtCfg.P2P.AddrBookStrict = false
		cmtCfg.P2P.AllowDuplicateIP = true

		var mnemonic string
		if i < len(cfg.Mnemonics) {
			mnemonic = cfg.Mnemonics[i]
		}

		nodeID, pubKey, err := genutil.InitializeNodeValidatorFilesFromMnemonic(cmtCfg, mnemonic)
		if err != nil {
			return nil, err
		}

		nodeIDs[i] = nodeID
		valPubKeys[i] = pubKey

		kb, err := keyring.New(sdk.KeyringServiceName(), keyring.BackendTest, clientDir, buf, cfg.Codec, cfg.KeyringOptions...)
		if err != nil {
			return nil, err
		}

		keyringAlgos, _ := kb.SupportedAlgorithms()
		algo, err := keyring.NewSigningAlgoFromString(cfg.SigningAlgo, keyringAlgos)
		if err != nil {
			return nil, err
		}

		addr, secret, err := testutil.GenerateSaveCoinKey(kb, nodeDirName, mnemonic, true, algo)
		if err != nil {
			return nil, err
		}

		// if PrintMnemonic is set to true, we print the first validator node's secret to the network's logger
		// for debugging and manual testing
		if cfg.PrintMnemonic && i == 0 {
			printMnemonic(l, secret)
		}

		info := map[string]string{"secret": secret}
		infoBz, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}

		// save private key seed words
		err = writeFile(fmt.Sprintf("%v.json", "key_seed"), clientDir, infoBz)
		if err != nil {
			return nil, err
		}

		balances := sdk.NewCoins(
			sdk.NewCoin(fmt.Sprintf("%stoken", nodeDirName), cfg.AccountTokens),
			sdk.NewCoin(cfg.BondDenom, cfg.StakingTokens),
		)

		genFiles = append(genFiles, cmtCfg.GenesisFile())
		genBalances = append(genBalances, banktypes.Balance{Address: addr.String(), Coins: balances.Sort()})
		genAccounts = append(genAccounts, authtypes.NewBaseAccount(addr, nil, 0, 0))

		commission, err := sdkmath.LegacyNewDecFromStr("0.5")
		if err != nil {
			return nil, err
		}

		createValMsg, err := stakingtypes.NewMsgCreateValidator(
			sdk.ValAddress(addr).String(),
			valPubKeys[i],
			sdk.NewCoin(cfg.BondDenom, cfg.BondedTokens),
			stakingtypes.NewDescription(nodeDirName, "", "", "", ""),
			stakingtypes.NewCommissionRates(commission, sdkmath.LegacyOneDec(), sdkmath.LegacyOneDec()),
			sdkmath.OneInt(),
		)
		if err != nil {
			return nil, err
		}

		p2pURL, err := url.Parse(p2pAddr)
		if err != nil {
			return nil, err
		}

		memo := fmt.Sprintf("%s@%s:%s", nodeIDs[i], p2pURL.Hostname(), p2pURL.Port())
		fee := sdk.NewCoins(sdk.NewCoin(fmt.Sprintf("%stoken", nodeDirName), sdkmath.NewInt(0)))
		txBuilder := cfg.TxConfig.NewTxBuilder()
		err = txBuilder.SetMsgs(createValMsg)
		if err != nil {
			return nil, err
		}
		txBuilder.SetFeeAmount(fee)    // Arbitrary fee
		txBuilder.SetGasLimit(1000000) // Need at least 100386
		txBuilder.SetMemo(memo)

		txFactory := tx.Factory{}
		txFactory = txFactory.
			WithChainID(cfg.ChainID).
			WithMemo(memo).
			WithKeybase(kb).
			WithTxConfig(cfg.TxConfig)

		err = tx.Sign(context.Background(), txFactory, nodeDirName, txBuilder, true)
		if err != nil {
			return nil, err
		}

		txBz, err := cfg.TxConfig.TxJSONEncoder()(txBuilder.GetTx())
		if err != nil {
			return nil, err
		}
		err = writeFile(fmt.Sprintf("%v.json", nodeDirName), gentxsDir, txBz)
		if err != nil {
			return nil, err
		}
		srvconfig.WriteConfigFile(filepath.Join(nodeDir, "config", "app.toml"), appCfg)

		clientCtx := client.Context{}.
			WithKeyringDir(clientDir).
			WithKeyring(kb).
			WithHomeDir(cmtCfg.RootDir).
			WithChainID(cfg.ChainID).
			WithInterfaceRegistry(cfg.InterfaceRegistry).
			WithCodec(cfg.Codec).
			WithLegacyAmino(cfg.LegacyAmino).
			WithTxConfig(cfg.TxConfig).
			WithAccountRetriever(cfg.AccountRetriever).
			WithNodeURI(cmtCfg.RPC.ListenAddress)

		// Provide ChainID here since we can't modify it in the Comet config.
		ctx.Viper.Set(flags.FlagChainID, cfg.ChainID)

		network.Validators[i] = &Validator{
			AppConfig:  appCfg,
			ClientCtx:  clientCtx,
			Ctx:        ctx,
			Dir:        filepath.Join(network.BaseDir, nodeDirName),
			NodeID:     nodeID,
			PubKey:     pubKey,
			Moniker:    nodeDirName,
			RPCAddress: cmtCfg.RPC.ListenAddress,
			P2PAddress: cmtCfg.P2P.ListenAddress,
			APIAddress: apiAddr,
			Address:    addr,
			ValAddress: sdk.ValAddress(addr),
		}
	}

	err := initGenFiles(cfg, genAccounts, genBalances, genFiles)
	if err != nil {
		return nil, err
	}
	err = collectGenFiles(cfg, network.Validators, network.BaseDir)
	if err != nil {
		return nil, err
	}

	l.Log("starting test network...")
	for idx, v := range network.Validators {
		if err := startInProcess(cfg, v); err != nil {
			return nil, err
		}
		l.Log("started validator", idx)
	}

	height, err := network.LatestHeight()
	if err != nil {
		return nil, err
	}

	l.Log("started test network at height:", height)

	// Ensure we cleanup incase any test was abruptly halted (e.g. SIGINT) as any
	// defer in a test would not be called.
	trapSignal(network.Cleanup)

	return network, nil
}

// trapSignal traps SIGINT and SIGTERM and calls os.Exit once a signal is received.
func trapSignal(cleanupFunc func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs

		if cleanupFunc != nil {
			cleanupFunc()
		}
		exitCode := 128

		switch sig {
		case syscall.SIGINT:
			exitCode += int(syscall.SIGINT)
		case syscall.SIGTERM:
			exitCode += int(syscall.SIGTERM)
		}

		os.Exit(exitCode)
	}()
}

// LatestHeight returns the latest height of the network or an error if the
// query fails or no validators exist.
func (n *Network) LatestHeight() (int64, error) {
	if len(n.Validators) == 0 {
		return 0, errors.New("no validators available")
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	timeout := time.NewTimer(time.Second * 5)
	defer timeout.Stop()

	var latestHeight int64
	val := n.Validators[0]
	queryClient := cmtservice.NewServiceClient(val.ClientCtx)

	for {
		select {
		case <-timeout.C:
			return latestHeight, errors.New("timeout exceeded waiting for block")
		case <-ticker.C:
			done := make(chan struct{})
			go func() {
				res, err := queryClient.GetLatestBlock(context.Background(), &cmtservice.GetLatestBlockRequest{})
				if err == nil && res != nil {
					latestHeight = res.SdkBlock.Header.Height
				}
				done <- struct{}{}
			}()
			select {
			case <-timeout.C:
				return latestHeight, errors.New("timeout exceeded waiting for block")
			case <-done:
				if latestHeight != 0 {
					return latestHeight, nil
				}
			}
		}
	}
}

// WaitForHeight performs a blocking check where it waits for a block to be
// committed after a given block. If that height is not reached within a timeout,
// an error is returned. Regardless, the latest height queried is returned.
func (n *Network) WaitForHeight(h int64) (int64, error) {
	return n.WaitForHeightWithTimeout(h, 20*time.Second)
}

// WaitForHeightWithTimeout is the same as WaitForHeight except the caller can
// provide a custom timeout.
func (n *Network) WaitForHeightWithTimeout(h int64, t time.Duration) (int64, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	timeout := time.NewTimer(t)
	defer timeout.Stop()

	if len(n.Validators) == 0 {
		return 0, errors.New("no validators available")
	}

	var latestHeight int64
	val := n.Validators[0]
	queryClient := cmtservice.NewServiceClient(val.ClientCtx)

	for {
		select {
		case <-timeout.C:
			return latestHeight, errors.New("timeout exceeded waiting for block")
		case <-ticker.C:

			res, err := queryClient.GetLatestBlock(context.Background(), &cmtservice.GetLatestBlockRequest{})
			if err == nil && res != nil {
				latestHeight = res.GetSdkBlock().Header.Height
				if latestHeight >= h {
					return latestHeight, nil
				}
			}
		}
	}
}

// RetryForBlocks will wait for the next block and execute the function provided.
// It will do this until the function returns a nil error or until the number of
// blocks has been reached.
func (n *Network) RetryForBlocks(retryFunc func() error, blocks int) error {
	for i := range blocks {
		_ = n.WaitForNextBlock() // ignore the error as we use the retry for validation
		err := retryFunc()
		if err == nil {
			return nil
		}
		// we've reached the last block to wait, return the error
		if i == blocks-1 {
			return err
		}
	}
	return nil
}

// WaitForNextBlock waits for the next block to be committed, returning an error
// upon failure.
func (n *Network) WaitForNextBlock() error {
	lastBlock, err := n.LatestHeight()
	if err != nil {
		return err
	}

	_, err = n.WaitForHeight(lastBlock + 1)
	if err != nil {
		return err
	}

	return err
}

// Cleanup removes the root testing (temporary) directory and stops both the
// CometBFT and API services. It allows other callers to create and start
// test networks. This method must be called when a test is finished, typically
// in a defer.
func (n *Network) Cleanup() {
	defer func() {
		lock.Unlock()
		n.Logger.Log("released test network lock")
	}()

	n.Logger.Log("cleaning up test network...")

	for _, v := range n.Validators {
		// cancel the validator's context which will signal to the gRPC and API
		// goroutines that they should gracefully exit.
		v.cancelFn()

		if err := v.errGroup.Wait(); err != nil {
			n.Logger.Log("unexpected error waiting for validator gRPC and API processes to exit", "err", err)
		}

		if v.tmNode != nil && v.tmNode.IsRunning() {
			if err := v.tmNode.Stop(); err != nil {
				n.Logger.Log("failed to stop validator CometBFT node", "err", err)
			}
		}

		if v.grpcWeb != nil {
			_ = v.grpcWeb.Close()
		}

		if v.app != nil {
			if err := v.app.Close(); err != nil {
				n.Logger.Log("failed to stop validator ABCI application", "err", err)
			}
		}
	}

	time.Sleep(100 * time.Millisecond)

	if n.Config.CleanupDir {
		_ = os.RemoveAll(n.BaseDir)
	}

	n.Logger.Log("finished cleaning up test network")
}

// printMnemonic prints a provided mnemonic seed phrase on a network logger
// for debugging and manual testing
func printMnemonic(l Logger, secret string) {
	lines := []string{
		"THIS MNEMONIC IS FOR TESTING PURPOSES ONLY",
		"DO NOT USE IN PRODUCTION",
		"",
		strings.Join(strings.Fields(secret)[0:8], " "),
		strings.Join(strings.Fields(secret)[8:16], " "),
		strings.Join(strings.Fields(secret)[16:24], " "),
	}

	lineLengths := make([]int, len(lines))
	for i, line := range lines {
		lineLengths[i] = len(line)
	}

	maxLineLength := 0
	for _, lineLen := range lineLengths {
		if lineLen > maxLineLength {
			maxLineLength = lineLen
		}
	}

	l.Log("\n")
	l.Log(strings.Repeat("+", maxLineLength+8))
	for _, line := range lines {
		l.Logf("++  %s  ++\n", centerText(line, maxLineLength))
	}
	l.Log(strings.Repeat("+", maxLineLength+8))
	l.Log("\n")
}

// centerText centers text across a fixed width, filling either side with whitespace buffers
func centerText(text string, width int) string {
	textLen := len(text)
	leftBuffer := strings.Repeat(" ", (width-textLen)/2)
	rightBuffer := strings.Repeat(" ", (width-textLen)/2+(width-textLen)%2)

	return fmt.Sprintf("%s%s%s", leftBuffer, text, rightBuffer)
}
