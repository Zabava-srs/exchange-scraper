package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fasthttp/router"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valyala/fasthttp"
)

var (
	exchangeApiUrls = map[string]string{
		"binance.com": "https://api.binance.com/api/v3/ticker/price",
		"gate.io":     "https://api.gateio.ws/api/v4/spot/tickers",
		"okx.com":     "https://www.okx.com/api/v5/market/tickers?instType=SPOT",
	}
	pgConn  = dbConnect()
	connStr = os.Getenv("pgConnectionString")
)

const (
	md5HealthCheckValue = "37c72e39a9cb88b7964daac6411f404b"
)

type binanceCom []struct {
	Pair  string `json:"symbol"`
	Price string `json:"price"`
}

type gateIo []struct {
	Pair  string `json:"currency_pair"`
	Price string `json:"last"`
}

type okxCom struct {
	Data []struct {
		Pair  string `json:"instId"`
		Price string `json:"last"`
	} `json:"data"`
}

type baseExchange []struct {
	Pair  string
	Price string
}

type jsonOutput struct {
	Exchange string `json:"exchange"`
	Pair     string `json:"pair"`
	Price    string `json:"price"`
}

func dbConnect() *pgxpool.Pool {
	log.Println("Attempting to connect to the DB...")
	pgConn, err := pgxpool.New(context.Background(), connStr)
	pgConn.Config().MinConns = 5
	pgConn.Config().MaxConns = 15
	if err != nil {
		log.Fatalf(fmt.Sprintf("Unable to connect to DB: %v", err))
	}
	return pgConn
}

func dbHealthCheckQuery() bool {
	var healthCheck string
	err := pgConn.QueryRow(context.Background(), "select md5('healthcheck')").Scan(&healthCheck)
	if err != nil {
		log.Printf("Healthcheck result - DB is unavailable: %v", err)
		return false
	} else if strings.Contains(healthCheck, md5HealthCheckValue) {
		return true
	} else {
		log.Printf("Healthcheck result - DB is unavailable: %v", err)
		return false
	}
}

func dbBatchRequest(queries []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	batch := &pgx.Batch{}
	tx, err := pgConn.Begin(ctx)
	if err != nil {
		log.Printf("Failed to start transaction: %v", err)
		return false
	}
	for i := 0; i < len(queries); i++ {
		batch.Queue(queries[i])
	}
	br := tx.SendBatch(ctx, batch)
	br.Close()
	err = tx.Commit(ctx)
	if err != nil {
		log.Printf("Failed to commit transaction: %v", err)
		return false
	}
	return true
}

func getApiJson(exchange string, apiUrl string) []byte {
	client := &http.Client{}
	req, err := http.NewRequest("GET", apiUrl, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/104.0.0.0 Safari/537.36")
	if err != nil {
		log.Printf("Request to ", exchange, "creating error:", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Request to ", exchange, "executing error:", err)
	}
	body_resp, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Body ", exchange, " read error:", err)
	}
	return body_resp
}

func getObjData(exchangeName string, exchangeApiUrl string) baseExchange {
	var retVal baseExchange
	switch exchangeName {
	case "gate.io":
		var gateIoObj gateIo
		resp_body := getApiJson(exchangeName, exchangeApiUrl)
		if err := json.Unmarshal(resp_body, &gateIoObj); err != nil {
			log.Printf("Can`t unmarshal ", exchangeName, " JSON ", err)
		}
		retVal = baseExchange(gateIoObj)
	case "binance.com":
		var binanceComObj binanceCom
		resp_body := getApiJson(exchangeName, exchangeApiUrl)
		if err := json.Unmarshal(resp_body, &binanceComObj); err != nil {
			log.Printf("Can`t unmarshal ", exchangeName, " JSON", err)
		}
		retVal = baseExchange(binanceComObj)
	case "okx.com":
		var okxComObj okxCom
		resp_body := getApiJson(exchangeName, exchangeApiUrl)
		if err := json.Unmarshal(resp_body, &okxComObj); err != nil {
			log.Printf("Can`t unmarshal ", exchangeName, " JSON", err)
		}
		retVal = baseExchange(okxComObj.Data)
	default:
		log.Printf("Case error! Exchange name not found")
	}
	return retVal
}

func requestsWorker() {
	for {
		//request frequency
		time.Sleep(15 * time.Second)
		for exchangeName, exchangeApiUrl := range exchangeApiUrls {
			batch := []string{}
			for _, v := range getObjData(exchangeName, exchangeApiUrl) {
				separatorDelete := regexp.MustCompile(`-|_`)
				parsedPair := separatorDelete.ReplaceAllString(v.Pair, "")
				parsedPrice, err := strconv.ParseFloat(v.Price, 32)
				if err != nil {
					log.Printf("Pair price convert error ", err)
					continue
				}
				query := fmt.Sprintf("insert into exchange_scraped_data (exchange, pair, price) values('%s', '%s', %f);", exchangeName, parsedPair, parsedPrice)
				batch = append(batch, query)
			}
			go dbBatchRequest(batch)
		}
	}
}

func healthCheck(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("text/plain")
	ctx.SetStatusCode(fasthttp.StatusOK)
	fmt.Fprintf(ctx, "I`m alive!")
}

func currentPairPrice(ctx *fasthttp.RequestCtx) {
	var data []jsonOutput
	for exchangeName, exchangeApiUrl := range exchangeApiUrls {
		for _, v := range getObjData(exchangeName, exchangeApiUrl) {
			separatorDelete := regexp.MustCompile(`-|_`)
			parsedPair := separatorDelete.ReplaceAllString(v.Pair, "")
			if ctx.UserValue("pair_price").(string) == parsedPair {
				data = append(data, jsonOutput{Exchange: exchangeName, Pair: parsedPair, Price: v.Price})
			}
		}
	}
	jsonReady, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		ctx.SetContentType("text/plain")
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		fmt.Fprintf(ctx, "Bad request, check uri! Error: %v", err)
	} else {
		ctx.SetContentType("application/json")
		ctx.SetStatusCode(fasthttp.StatusOK)
		fmt.Fprintf(ctx, "%s", string(jsonReady))
	}
}

func routesDeclare(r *router.Router) {
	log.Println("Server ready!")
	r.GET("/healthcheck", healthCheck)
	r.GET("/pair/{pair_price}/price", currentPairPrice)
}

func main() {
	loc, err := time.LoadLocation("Asia/Yekaterinburg")
	if err != nil {
		log.Printf("%s", err)
	}
	time.Local = loc
	if dbHealthCheckQuery() {
		log.Println("API successfully connected to DB")
	} else {
		log.Fatalf(fmt.Sprintf("Unable to connect to DB: %v", err))
	}
	go requestsWorker()
	log.Println("Server starting...")
	r := router.New()
	routesDeclare(r)
	log.Fatalln(fasthttp.ListenAndServe(":80", r.Handler))
}
