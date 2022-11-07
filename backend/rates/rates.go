// Copyright 2018 Shift Devices AG
// Copyright 2020 Shift Crypto AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/digitalbitbox/bitbox-wallet-app/util/errp"
	"github.com/digitalbitbox/bitbox-wallet-app/util/logging"
	"github.com/digitalbitbox/bitbox-wallet-app/util/observable"
	"github.com/digitalbitbox/bitbox-wallet-app/util/observable/action"
	"github.com/digitalbitbox/bitbox-wallet-app/util/ratelimit"
	"github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
)

const (
	// Latest rates are fetched for all these (coin, fiat) pairs.
	simplePriceAllIDs        = "bitcoin,litecoin,ethereum,basic-attention-token,dai,chainlink,maker,sai,usd-coin,tether,0x,wrapped-bitcoin,pax-gold"
	simplePriceAllCurrencies = "usd,eur,chf,gbp,jpy,krw,cny,rub,cad,aud,ils,btc,sgd,hkd,brl,nok,sek"
	// RatesEventSubject is the Subject of the event generated by new rates fetching.
	RatesEventSubject = "rates"
)

const interval = time.Minute

type exchangeRate struct {
	value     float64
	timestamp time.Time
}

// Fiat type represents currency strings.
type Fiat string

// String return a string representing the Fiat.
func (f Fiat) String() string {
	return string(f)
}

// supported Fiat.
const (
	AUD Fiat = "AUD"
	BRL Fiat = "BRL"
	CAD Fiat = "CAD"
	CHF Fiat = "CHF"
	CNY Fiat = "CNY"
	EUR Fiat = "EUR"
	GBP Fiat = "GBP"
	HKD Fiat = "HKD"
	ILS Fiat = "ILS"
	JPY Fiat = "JPY"
	KRW Fiat = "KRW"
	NOK Fiat = "NOK"
	RUB Fiat = "RUB"
	SEK Fiat = "SEK"
	SGD Fiat = "SGD"
	USD Fiat = "USD"
	BTC Fiat = "BTC"
)

// RateUpdater provides cryptocurrency-to-fiat conversion rates.
type RateUpdater struct {
	observable.Implementation

	httpClient *http.Client
	log        *logrus.Entry

	// last contains most recent conversion to fiat, keyed by a coin.
	last map[string]map[string]float64
	// stopLastUpdateLoop is the cancel function of the lastUpdateLoop context.
	stopLastUpdateLoop context.CancelFunc

	// historyDB is an internal cached copy of history, transparent to the users.
	// While RateUpdater can function without a valid historyDB,
	// it may be impacted by API rate limits.
	historyDB *bbolt.DB

	historyMu sync.RWMutex // guards both history and historyGo
	// history contains historical conversion rates in asc order, keyed by coin+fiat pair.
	// For example, BTC/CHF pair's key is "btcCHF".
	history map[string][]exchangeRate
	// historyGo contains context canceling funcs to stop periodic updates
	// of historical data, keyed by coin+fiat pair.
	// For example, BTC/EUR pair's key is "btcEUR".
	historyGo map[string]context.CancelFunc

	// CoinGecko is where updater gets the historical conversion rates.
	// See https://www.coingecko.com/en/api for details.
	coingeckoURL string
	// All requests to coingeckoURL are rate-limited using geckoLimiter.
	geckoLimiter *ratelimit.LimitedCall
}

// NewRateUpdater returns a new rates updater.
// The dbdir argument is the location of a historical rates database cache.
// The returned updater can function without a valid database cache but may be
// impacted by rate limits. The database cache is transparent to the updater users.
// To stay within acceptable rate limits defined by CoinGeckoRateLimit, callers can
// use util/ratelimit package.
//
// Both Last and PriceAt of the newly created updater always return zero values
// until data is fetched from the external APIs. To make the updater start fetching data
// the caller can use StartCurrentRates and ReconfigureHistory, respectively.
//
// The caller is advised to always call Stop as soon as the updater is no longer needed
// to free up all used resources.
func NewRateUpdater(client *http.Client, dbdir string) *RateUpdater {
	log := logging.Get().WithGroup("rates")
	db, err := openRatesDB(dbdir, log)
	if err != nil {
		log.Errorf("openRatesDB(%q): %v; database is unusable", dbdir, err)
		// To avoid null pointer dereference in other methods where historyDB
		// is used, use an unopened DB instance. This simplifies code, reducing
		// the number of nil checks and an additional mutex.
		// An unopened DB will simply return bbolt.ErrDatabaseNotOpen on all operations.
		db = &bbolt.DB{}
	}
	apiURL := shiftGeckoMirrorAPIV3
	return &RateUpdater{
		last:         make(map[string]map[string]float64),
		history:      make(map[string][]exchangeRate),
		historyGo:    make(map[string]context.CancelFunc),
		historyDB:    db,
		log:          log,
		httpClient:   client,
		coingeckoURL: apiURL,
		geckoLimiter: ratelimit.NewLimitedCall(apiRateLimit(apiURL)),
	}
}

// SetCoingeckoURL overrides the default URL the rates updater connects to. Useful for testing.
func (updater *RateUpdater) SetCoingeckoURL(url string) {
	updater.coingeckoURL = url
}

// LatestPrice returns the most recent conversion rates.
// The returned map is keyed by a crypto coin with values mapped by fiat rates.
// RateUpdater assumes the returned value is never modified by the callers.
func (updater *RateUpdater) LatestPrice() map[string]map[string]float64 {
	return updater.last
}

// LatestPriceForPair returns the conversion rate for the given (coin, fiat) pair. Returns an error
// if the rates have not been fetched yet. `coinUnit` values are the same as `coin.Unit`.
func (updater *RateUpdater) LatestPriceForPair(coinUnit, fiat string) (float64, error) {
	// TODO: use coin.Code
	last := updater.LatestPrice()
	if last == nil {
		return 0, errp.New("rates not available yet")
	}
	return last[coinUnit][fiat], nil
}

// HistoricalPriceAt returns a historical exchange rate for the given coin.
// The returned value may be imprecise if at arg matches no timestamp exactly.
// In this case, linear interpolation is used as an approximation.
// If no data is available with the given args, HistoricalPriceAt returns 0.
// The latest rates can lag behind by many minutes (5-30min). Use `LatestPrice` get get the latest
// rates.
func (updater *RateUpdater) HistoricalPriceAt(coin, fiat string, at time.Time) float64 {
	updater.historyMu.RLock()
	defer updater.historyMu.RUnlock()
	data := updater.history[coin+fiat]
	if len(data) == 0 {
		return 0 // no data at all
	}
	// Find an index of the first entry older or equal the at timestamp.
	idx := sort.Search(len(data), func(i int) bool {
		return !data[i].timestamp.Before(at)
	})
	if idx == len(data) || (idx == 0 && !data[idx].timestamp.Equal(at)) {
		return 0 // no data
	}
	if data[idx].timestamp.Equal(at) {
		return data[idx].value // don't need to interpolate
	}

	// Approximate value, somewhere between a and b.
	// https://en.wikipedia.org/wiki/Linear_interpolation#Linear_interpolation_as_approximation
	a := data[idx-1]
	b := data[idx]
	x := float64((at.Unix() - a.timestamp.Unix())) / float64((b.timestamp.Unix() - a.timestamp.Unix()))
	return a.value + x*(b.value-a.value)
}

// StartCurrentRates spins up the updater's goroutines to periodically update
// current exchange rates. It returns immediately.
// StartCurrentRates panics if called twice, even after Stop'ed.
//
// To initiate historical exchange rates update, the caller can use ReconfigureHistory.
// The current and historical exchange rates are independent from each other.
//
// StartCurrentRates is unsafe for concurrent use.
func (updater *RateUpdater) StartCurrentRates() {
	if updater.stopLastUpdateLoop != nil {
		panic("RateUpdater: StartCurrentRates called twice")
	}
	ctx, cancel := context.WithCancel(context.Background())
	updater.stopLastUpdateLoop = cancel
	go updater.lastUpdateLoop(ctx)
}

// Stop shuts down all running goroutines and closes history database cache.
// It may return before the goroutines have exited.
// Once Stop'ed, the updater is no longer usable.
//
// Stop is unsafe for concurrent use.
func (updater *RateUpdater) Stop() {
	updater.stopAllHistory()
	if updater.stopLastUpdateLoop != nil {
		updater.stopLastUpdateLoop()
	}
	if err := updater.historyDB.Close(); err != nil {
		updater.log.Errorf("historyDB.Close: %v", err)
	}
}

// lastUpdateLoop periodically updates most recent exchange rates.
// It never returns until the context is done.
func (updater *RateUpdater) lastUpdateLoop(ctx context.Context) {
	for {
		updater.updateLast(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			// continue
		}
	}
}

func (updater *RateUpdater) updateLast(ctx context.Context) {
	param := url.Values{
		"ids":           {simplePriceAllIDs},
		"vs_currencies": {simplePriceAllCurrencies},
	}
	endpoint := fmt.Sprintf("%s/simple/price?%s", updater.coingeckoURL, param.Encode())
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		updater.log.WithError(err).Error("could not create request")
		updater.last = nil
		return
	}

	var geckoRates map[string]map[string]float64
	callErr := updater.geckoLimiter.Call(ctx, "updateLast", func() error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res, err := updater.httpClient.Do(req.WithContext(ctx))
		if err != nil {
			return errp.WithStack(err)
		}
		defer res.Body.Close() //nolint:errcheck
		if res.StatusCode != http.StatusOK {
			return errp.Newf("bad response code %d", res.StatusCode)
		}
		const max = 10240
		responseBody, err := ioutil.ReadAll(io.LimitReader(res.Body, max+1))
		if err != nil {
			return errp.WithStack(err)
		}
		if len(responseBody) > max {
			return errp.Newf("rates response too long (> %d bytes)", max)
		}
		if err := json.Unmarshal(responseBody, &geckoRates); err != nil {
			return errp.WithMessage(err,
				fmt.Sprintf("could not parse rates response: %s", string(responseBody)))
		}
		return nil
	})
	if callErr != nil {
		updater.log.WithError(callErr).Errorf("updateLast")
		updater.last = nil
		return
	}
	// Convert the map with coingecko coin/fiat codes to a map of coin/fiat units.
	rates := map[string]map[string]float64{}
	for coin, val := range geckoRates {
		coinUnit := geckoCoinToUnit[coin]
		if coinUnit == "" {
			updater.log.Errorf("unsupported CoinGecko coin: %s", coin)
			continue
		}
		newVal := map[string]float64{}
		for geckoFiat, rates := range val {
			fiat, ok := fromGeckoFiat[geckoFiat]
			if !ok {
				updater.log.Errorf("unsupported fiat: %s", geckoFiat)
				continue
			}
			newVal[fiat] = rates
		}
		rates[coinUnit] = newVal
	}

	// Provide conversion rates for testnets as well, useful for testing.
	for _, testnetUnit := range []string{"TBTC", "RBTC", "TLTC", "GOETH"} {
		if testnetUnit == "GOETH" {
			rates[testnetUnit] = rates[testnetUnit[2:]]
		} else {
			rates[testnetUnit] = rates[testnetUnit[1:]]
		}
	}

	if reflect.DeepEqual(rates, updater.last) {
		return
	}
	updater.last = rates
	updater.Notify(observable.Event{
		Subject: RatesEventSubject,
		Action:  action.Replace,
		Object:  rates,
	})
}
