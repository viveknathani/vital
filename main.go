package main

import (
	_ "embed"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/warthog618/go-gpiocdev"
)

type Config struct {
	ChipName              string
	LineOffset            int
	CircumferenceInMetres float64
	HttpPort              string
	BodyWeightKilograms   float64
	IdleTimeoutSeconds    float64
}

type Session struct {
	TotalRevolutions      uint64
	StartTimeEpochSeconds int64
	LastTimestamp         time.Duration
	LastInterval          time.Duration

	LastPulseWall time.Time
	LastCalcWall  time.Time
	MovingSeconds float64
	KiloCalories  float64
}

type Stats struct {
	SpeedKilometresPerHour float64 `json:"speedKilometresPerHour"`
	TotalRevolutions       uint64  `json:"totalRevolutions"`
	DistanceKilometres     float64 `json:"distanceKilometres"`
	StartTimeEpochSeconds  int64   `json:"startTimeEpochSeconds"`
	MovingMinutes          float64 `json:"movingMinutes"`
	KiloCalories           float64 `json:"kiloCalories"`
}

type ApiResponse struct {
	Data    any    `json:"data"`
	Message string `json:"message"`
}

type App struct {
	Config  Config
	Session Session
	Line    *gpiocdev.Line
	guard   chan struct{}
}

func NewApp(cfg Config) *App {
	return &App{
		Config:  cfg,
		Session: Session{StartTimeEpochSeconds: time.Now().Unix()},
		guard:   make(chan struct{}, 1),
	}
}

func (app *App) lock()   { app.guard <- struct{}{} }
func (app *App) unlock() { <-app.guard }

func metFromSpeed(speedKmh float64) float64 {
	switch {
	case speedKmh < 10:
		return 3.5
	case speedKmh < 16:
		return 5.5
	case speedKmh < 20:
		return 7.0
	case speedKmh < 24:
		return 8.0
	case speedKmh < 28:
		return 10.0
	default:
		return 12.0
	}
}

func (app *App) onEdge(event gpiocdev.LineEvent) {
	if event.Type != gpiocdev.LineEventFallingEdge {
		return
	}

	eventTimestamp := event.Timestamp

	app.lock()
	defer app.unlock()

	if app.Session.LastTimestamp > 0 {
		dt := eventTimestamp - app.Session.LastTimestamp
		if dt <= 10*time.Millisecond {
			app.Session.LastTimestamp = eventTimestamp
			return
		}
		app.Session.LastInterval = dt
		app.Session.TotalRevolutions++
	} else {
		// first ever pulse
		app.Session.TotalRevolutions++
	}
	app.Session.LastTimestamp = eventTimestamp
	app.Session.LastPulseWall = time.Now()
}

func (app *App) snapshot() Stats {
	app.lock()
	defer app.unlock()

	now := time.Now()
	dtWall := 0.0
	if !app.Session.LastCalcWall.IsZero() {
		dtWall = now.Sub(app.Session.LastCalcWall).Seconds()
	}
	app.Session.LastCalcWall = now

	// Distance
	distanceKm := float64(app.Session.TotalRevolutions) * app.Config.CircumferenceInMetres / 1000.0

	// Instantaneous speed from last interval
	var speedKmh float64
	if app.Session.LastInterval > 0 {
		dtNs := float64(app.Session.LastInterval.Nanoseconds())
		speedKmh = app.Config.CircumferenceInMetres * 3.6e9 / dtNs
	}

	// Moving?
	moving := false
	if !app.Session.LastPulseWall.IsZero() {
		if time.Since(app.Session.LastPulseWall).Seconds() < app.Config.IdleTimeoutSeconds {
			moving = true
		}
	}

	// Update kcal + moving time only if moving
	if moving && dtWall > 0 {
		met := metFromSpeed(speedKmh)
		kcalPerMin := (met * 3.5 * app.Config.BodyWeightKilograms) / 200.0
		app.Session.KiloCalories += kcalPerMin * (dtWall / 60.0)
		app.Session.MovingSeconds += dtWall
	}

	return Stats{
		SpeedKilometresPerHour: round(speedKmh, 2),
		TotalRevolutions:       app.Session.TotalRevolutions,
		DistanceKilometres:     round(distanceKm, 3),
		StartTimeEpochSeconds:  app.Session.StartTimeEpochSeconds,
		MovingMinutes:          round(app.Session.MovingSeconds/60.0, 2),
		KiloCalories:           round(app.Session.KiloCalories, 1),
	}
}

func round(v float64, places int) float64 {
	if places < 0 {
		return v
	}
	f := math.Pow(10, float64(places))
	return math.Round(v*f) / f
}

func (a *App) reset() {
	a.lock()
	a.Session = Session{StartTimeEpochSeconds: time.Now().Unix()}
	a.unlock()
}

func (a *App) openGPIO() error {
	options := []gpiocdev.LineReqOption{
		gpiocdev.AsInput,
		gpiocdev.WithPullUp,
		gpiocdev.WithBothEdges,
		gpiocdev.WithEventHandler(a.onEdge),
	}
	options = append(options, gpiocdev.WithMonotonicEventClock)

	line, err := gpiocdev.RequestLine(a.Config.ChipName, a.Config.LineOffset, options...)
	if err != nil {
		return err
	}
	a.Line = line
	return nil
}

func (a *App) closeGPIO() {
	if a.Line != nil {
		_ = a.Line.Close()
	}
}

//go:embed index.html
var indexHTML string

func main() {
	config := Config{
		ChipName:              "gpiochip0",
		LineOffset:            17,
		CircumferenceInMetres: 1.41,
		HttpPort:              "80",
		BodyWeightKilograms:   85,
		IdleTimeoutSeconds:    2.0,
	}

	app := NewApp(config)
	if err := app.openGPIO(); err != nil {
		log.Fatalf("gpio: %v", err)
	}
	defer app.closeGPIO()

	server := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		AppName:               "vital",
	})

	server.Get("/api/v1/stats", func(c *fiber.Ctx) error {
		return c.JSON(ApiResponse{Data: app.snapshot(), Message: "ok"})
	})

	server.Post("/api/v1/reset", func(c *fiber.Ctx) error {
		app.reset()
		return c.JSON(ApiResponse{Data: fiber.Map{}, Message: "reset done"})
	})

	server.Get("/", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(indexHTML)
	})

	go func() {
		if err := server.Listen(":" + config.HttpPort); err != nil {
			log.Printf("server exit: %v", err)
		}
	}()

	log.Println("vital is running! 🚴")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals
	_ = server.Shutdown()
}
