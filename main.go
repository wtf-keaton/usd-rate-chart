package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"
	"golang.org/x/net/html/charset"
)

const (
	dailyRatesURL   = "https://www.cbr.ru/scripts/XML_daily.asp"
	dynamicRatesURL = "https://www.cbr.ru/scripts/XML_dynamic.asp?date_req1=%s&date_req2=%s&VAL_NM_RQ=R01235"
	httpTimeout     = 5 * time.Second
	cacheExpiration = 1 * time.Hour
)

type ValCurs struct {
	Date    string   `xml:"Date,attr"`
	Valutes []Valute `xml:"Valute"`
	Records []Record `xml:"Record"`
}

type Record struct {
	Date  string `xml:"Date,attr"`
	Value string `xml:"Value"`
}

type Valute struct {
	CharCode string `xml:"CharCode"`
	Value    string `xml:"Value"`
}

type RateHistory struct {
	Date string  `json:"date"`
	Rate float64 `json:"rate"`
}

type Cache struct {
	mu         sync.RWMutex
	data       map[string]interface{}
	expiration map[string]time.Time
}

func NewCache() *Cache {
	return &Cache{
		data:       make(map[string]interface{}),
		expiration: make(map[string]time.Time),
	}
}

func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	exp, exists := c.expiration[key]
	if !exists || time.Now().After(exp) {
		return nil, false
	}

	val, exists := c.data[key]
	return val, exists
}

func (c *Cache) Set(key string, value interface{}, expiration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[key] = value
	c.expiration[key] = time.Now().Add(expiration)
}

type CurrencyService struct {
	client *http.Client
	cache  *Cache
}

func NewCurrencyService() *CurrencyService {
	return &CurrencyService{
		client: &http.Client{
			Timeout: httpTimeout,
		},
		cache: NewCache(),
	}
}

func (s *CurrencyService) fetchXML(url string) (*ValCurs, error) {
	if cachedData, found := s.cache.Get(url); found {
		return cachedData.(*ValCurs), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	decoder := xml.NewDecoder(resp.Body)
	decoder.CharsetReader = charset.NewReaderLabel

	var valCurs ValCurs
	if err := decoder.Decode(&valCurs); err != nil {
		return nil, fmt.Errorf("failed to parse XML: %w", err)
	}

	s.cache.Set(url, &valCurs, cacheExpiration)

	return &valCurs, nil
}

func (s *CurrencyService) GetDailyRates() (*ValCurs, error) {
	return s.fetchXML(dailyRatesURL)
}

func (s *CurrencyService) GetUsdRate() (float64, error) {
	valCurs, err := s.GetDailyRates()
	if err != nil {
		return 0, err
	}

	for _, valute := range valCurs.Valutes {
		if valute.CharCode == "USD" {
			return parseRate(valute.Value)
		}
	}

	return 0, errors.New("USD rate not found")
}

func (s *CurrencyService) GetCursDate() (string, error) {
	valCurs, err := s.GetDailyRates()
	if err != nil {
		return "", err
	}
	return valCurs.Date, nil
}

func (s *CurrencyService) GetUsdRatesHistory() ([]RateHistory, error) {
	now := time.Now()
	startDate := now.AddDate(0, 0, -7).Format("02.01.2006")
	endDate := now.Format("02.01.2006")

	url := fmt.Sprintf(dynamicRatesURL, startDate, endDate)
	valCurs, err := s.fetchXML(url)
	if err != nil {
		return nil, err
	}

	var history []RateHistory
	for _, record := range valCurs.Records {
		rate, err := parseRate(record.Value)
		if err != nil {
			continue
		}
		history = append(history, RateHistory{Date: record.Date, Rate: rate})
	}

	return history, nil
}

func parseRate(rateStr string) (float64, error) {
	rateStr = strings.Replace(rateStr, ",", ".", 1)
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse rate: %w", err)
	}
	return rate, nil
}

func setupRoutes(app *fiber.App, service *CurrencyService) {
	app.Get("/", func(c *fiber.Ctx) error {
		course, err := service.GetUsdRate()
		if err != nil {
			log.Printf("Error getting USD rate: %v", err)
			course = 0
		}

		date, err := service.GetCursDate()
		if err != nil {
			log.Printf("Error getting rate date: %v", err)
			date = ""
		}

		return c.Render("index", fiber.Map{
			"course": course,
			"date":   date,
		})
	})

	app.Get("/history", func(c *fiber.Ctx) error {
		history, err := service.GetUsdRatesHistory()
		if err != nil {
			log.Printf("Error getting rate history: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(history)
	})
}

func main() {
	engine := html.New("template", ".html")
	app := fiber.New(fiber.Config{
		Views: engine,
	})

	service := NewCurrencyService()
	setupRoutes(app, service)

	log.Println("Starting server on :3000")
	if err := app.Listen(":3000"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
