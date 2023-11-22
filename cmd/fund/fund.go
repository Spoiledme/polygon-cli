package fund

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/chenzhijie/go-web3"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	//go:embed usage.md
	usage                  string
	walletCount            int
	fundingWalletPK        string
	fundingWalletPublicKey *ecdsa.PublicKey
	chainRPC               string
	concurrencyLevel       int
	walletFundingAmt       float64
	walletFundingGas       uint64
	nonceMutex             sync.Mutex
	globalNonce            uint64
	nonceInitialized       bool
	outputFileFlag         string
)

// Wallet struct to hold public key, private key, and address
type Wallet struct {
	PublicKey  *ecdsa.PublicKey
	PrivateKey *ecdsa.PrivateKey
	Address    common.Address
}

func getChainIDFromNode(chainRPC string) (int64, error) {
	// Create an HTTP client
	client := &http.Client{}

	// Prepare the JSON-RPC request payload
	payload := `{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`

	// Create the HTTP request
	req, ReqErr := http.NewRequest("POST", chainRPC, strings.NewReader(payload))
	if ReqErr != nil {
		return 0, ReqErr
	}

	// Set the required headers
	req.Header.Set("Content-Type", "application/json")

	// Send the request
	resp, doErr := client.Do(req)
	if doErr != nil {
		return 0, doErr
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body) // Replace ioutil.ReadAll with io.ReadAll
	if readErr != nil {
		return 0, readErr
	}

	// Parse the JSON response
	var result map[string]interface{}
	if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
		return 0, jsonErr
	}

	// Extract the chain ID from the response
	chainIDHex, ok := result["result"].(string)
	if !ok {
		return 0, fmt.Errorf("unable to extract chain ID from response")
	}

	// Convert the chain ID from hex to int64
	int64ChainID, parseErr := strconv.ParseInt(chainIDHex, 0, 64)
	if parseErr != nil {
		return 0, parseErr
	}

	return int64ChainID, nil
}

func generateNonce(web3Client *web3.Web3) (uint64, error) {
	nonceMutex.Lock()
	defer nonceMutex.Unlock()

	if nonceInitialized {
		globalNonce++
	} else {
		// Derive the public key from the funding wallet's private key
		fundingWalletECDSA, ecdsaErr := crypto.HexToECDSA(fundingWalletPK)
		if ecdsaErr != nil {
			log.Error().Err(ecdsaErr).Msg("Error getting ECDSA from funding wallet private key")
			return 0, ecdsaErr
		}

		fundingWalletPublicKey = &fundingWalletECDSA.PublicKey
		// Convert ecdsa.PublicKey to common.Address
		fundingAddress := crypto.PubkeyToAddress(*fundingWalletPublicKey)

		nonce, err := web3Client.Eth.GetNonce(fundingAddress, nil)
		if err != nil {
			log.Error().Err(err).Msg("Error getting nonce")
			return 0, err
		}
		globalNonce = nonce
		nonceInitialized = true
	}

	return globalNonce, nil
}

func generateWallets(numWallets int) ([]Wallet, error) {
	wallets := make([]Wallet, numWallets)

	for i := 0; i < numWallets; i++ {
		account, err := crypto.GenerateKey()
		if err != nil {
			log.Error().Err(err).Msg("Error generating key")
			return nil, err
		}

		addr := crypto.PubkeyToAddress(account.PublicKey)
		wallet := Wallet{
			PublicKey:  &account.PublicKey,
			PrivateKey: account,
			Address:    addr,
		}

		wallets[i] = wallet
	}
	return wallets, nil
}

func fundWallets(web3Client *web3.Web3, wallets []Wallet, amountWei *big.Int, walletFundingGas uint64, concurrency int) error {
	// Create a channel to control concurrency
	walletChan := make(chan Wallet, len(wallets))
	for _, wallet := range wallets {
		walletChan <- wallet
	}
	close(walletChan)

	// Wait group to ensure all goroutines finish before returning
	var wg sync.WaitGroup
	wg.Add(concurrency)

	// Function to fund wallets
	fundWallet := func() {
		defer wg.Done()
		for wallet := range walletChan {
			nonce, err := generateNonce(web3Client)
			if err != nil {
				log.Error().Err(err).Msg("Error getting nonce")
				return
			}

			// Fund the wallet using the obtained nonce
			_, err = web3Client.Eth.SyncSendRawTransaction(
				wallet.Address,
				amountWei,
				nonce,
				walletFundingGas,
				web3Client.Utils.ToGWei(1),
				nil,
			)
			if err != nil {
				log.Error().Err(err).Str("wallet", wallet.Address.Hex()).Msg("Error funding wallet")
				return
			}

			log.Info().Str("wallet", wallet.Address.Hex()).Msgf("Funded with %s wei", amountWei.String())
		}
	}

	// Start funding the wallets concurrently
	for i := 0; i < concurrency; i++ {
		go fundWallet()
	}

	// Wait for all goroutines to finish
	wg.Wait()
	return nil
}

// fundCmd represents the fund command
var FundCmd = &cobra.Command{
	Use:   "fund",
	Short: "Bulk fund many crypto wallets automatically.",
	Long:  usage,
	Run: func(cmd *cobra.Command, args []string) {
		if err := runFunding(cmd); err != nil {
			log.Error().Err(err).Msg("Error funding wallets")
		}
	},
}

func runFunding(cmd *cobra.Command) error {
	// Capture the start time
	startTime := time.Now()

	// Remove '0x' prefix from fundingWalletPK if present
	fundingWalletPK = strings.TrimPrefix(fundingWalletPK, "0x")

	// setup new web3 session with remote rpc node
	web3Client, clientErr := web3.NewWeb3(chainRPC)
	if clientErr != nil {
		cmd.PrintErrf("There was an error creating web3 client: %s", clientErr.Error())
		return clientErr
	}

	// add pk to session for sending signed transactions
	if setAcctErr := web3Client.Eth.SetAccount(fundingWalletPK); setAcctErr != nil {
		cmd.PrintErrf("There was an error setting account with pk: %s", setAcctErr.Error())
		return setAcctErr
	}

	// Query the chain ID from the rpc node
	chainID, chainIDErr := getChainIDFromNode(chainRPC)
	if chainIDErr != nil {
		log.Error().Err(chainIDErr).Msg("Error getting chain ID")
		return chainIDErr
	}

	// Set proper chainId for corresponding chainRPC
	web3Client.Eth.SetChainId(chainID)

	// generate set of new wallet objects
	wallets, genWalletErr := generateWallets(walletCount)
	if genWalletErr != nil {
		cmd.PrintErrf("There was an error generating wallet objects: %s", genWalletErr.Error())
		return genWalletErr
	}

	// fund all crypto wallets
	log.Info().Msg("Starting to fund loadtest wallets...")
	fundWalletErr := fundWallets(web3Client, wallets, big.NewInt(int64(walletFundingAmt*1e18)), uint64(walletFundingGas), concurrencyLevel)
	if fundWalletErr != nil {
		log.Error().Err(fundWalletErr).Msg("Error funding wallets")
		return fundWalletErr
	}

	// Save wallet details to a file
	outputFile := outputFileFlag // You can modify the file format or name as needed

	type WalletDetails struct {
		Address    string `json:"Address"`
		PrivateKey string `json:"PrivateKey"`
	}

	walletDetails := make([]WalletDetails, len(wallets))
	for i, w := range wallets {
		privateKey := hex.EncodeToString(w.PrivateKey.D.Bytes()) // Convert private key to hex
		walletDetails[i] = WalletDetails{
			Address:    w.Address.Hex(),
			PrivateKey: privateKey,
		}
	}

	// Convert walletDetails to JSON
	walletsJSON, jsonErr := json.MarshalIndent(walletDetails, "", "  ")
	if jsonErr != nil {
		log.Error().Err(jsonErr).Msg("Error converting wallet details to JSON")
		return jsonErr
	}

	// Write JSON data to a file
	file, createErr := os.Create(outputFile)
	if createErr != nil {
		log.Error().Err(createErr).Msg("Error creating file")
		return createErr
	}
	defer file.Close()

	_, writeErr := file.Write(walletsJSON)
	if writeErr != nil {
		log.Error().Err(writeErr).Msg("Error writing wallet details to file")
		return writeErr
	}

	log.Info().Msgf("Wallet details have been saved to %s", outputFile)

	// Calculate the duration
	duration := time.Since(startTime)
	log.Info().Msgf("Total execution time: %s", duration)

	return nil
}

func init() {
	FundCmd.Flags().IntVar(&walletCount, "wallet-count", 2, "Number of wallets to fund")
	FundCmd.Flags().StringVar(&fundingWalletPK, "funding-wallet-pk", "", "Corresponding private key for funding wallet address, ensure you remove leading 0x")
	FundCmd.Flags().StringVar(&chainRPC, "rpc-url", "http://localhost:8545", "The RPC endpoint url")
	FundCmd.Flags().IntVar(&concurrencyLevel, "concurrency", 2, "Concurrency level for speeding up funding wallets")
	FundCmd.Flags().Float64Var(&walletFundingAmt, "wallet-funding-amt", 0.05, "Amount to fund each wallet with")
	FundCmd.Flags().Uint64Var(&walletFundingGas, "wallet-funding-gas", 100000, "Gas for each wallet funding transaction")
	FundCmd.Flags().StringVar(&outputFileFlag, "output-file", "funded_wallets.json", "Specify the output JSON file name")
}