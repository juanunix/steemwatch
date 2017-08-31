package server

import (
	"net"
	"net/url"

	"github.com/tchap/steemwatch/config"
	"github.com/tchap/steemwatch/server/auth"
	"github.com/tchap/steemwatch/server/auth/facebook"
	"github.com/tchap/steemwatch/server/auth/github"
	"github.com/tchap/steemwatch/server/auth/google"
	"github.com/tchap/steemwatch/server/auth/reddit"
	"github.com/tchap/steemwatch/server/context"
	"github.com/tchap/steemwatch/server/db"
	"github.com/tchap/steemwatch/server/routes/api/events/descendantpublished"
	"github.com/tchap/steemwatch/server/routes/api/eventstream"
	"github.com/tchap/steemwatch/server/routes/api/notifiers/slack"
	"github.com/tchap/steemwatch/server/routes/api/notifiers/steemitchat"
	"github.com/tchap/steemwatch/server/routes/api/profile"
	"github.com/tchap/steemwatch/server/routes/api/v1/info"
	"github.com/tchap/steemwatch/server/routes/home"
	"github.com/tchap/steemwatch/server/routes/logout"
	"github.com/tchap/steemwatch/server/sessions"
	"github.com/tchap/steemwatch/server/users/stores/mongodb"
	"github.com/tchap/steemwatch/server/views"

	"github.com/labstack/echo"
	"github.com/labstack/echo/engine"
	"github.com/labstack/echo/engine/fasthttp"
	"github.com/labstack/echo/middleware"
	"github.com/pkg/errors"
	"gopkg.in/mgo.v2"
	"gopkg.in/tomb.v2"
)

type Context struct {
	EventStreamManager *eventstream.Manager

	listener net.Listener

	t tomb.Tomb
}

func Run(mongo *mgo.Database, cfg *config.Config) (*Context, error) {
	serverCtx := &context.Context{}

	// Environment.
	switch cfg.Env {
	case "development":
		serverCtx.Env = context.EnvironmentDevelopment
	case "production":
		serverCtx.Env = context.EnvironmentProduction
		serverCtx.SSLEnabled = true
	default:
		return nil, errors.New("invalid environment: " + cfg.Env)
	}

	// Database.
	serverCtx.DB = mongo

	// User store.
	userStore := mongodb.NewUserStore(mongo.C("users"))

	// Session manager.
	hashKey, blockKey, err := getSecureCookieKeys(mongo)
	if err != nil {
		return nil, err
	}
	sessionManager, err := sessions.NewSessionManager(hashKey, blockKey, userStore)
	if err != nil {
		return nil, err
	}

	serverCtx.SessionManager = sessionManager

	// Server context.
	canonicalURL, err := url.Parse(cfg.CanonicalURL)
	if err != nil {
		return nil, errors.Wrap(err, "invalid canonical URL")
	}

	serverCtx.CanonicalURL = canonicalURL

	// Echo.
	e := echo.New()

	// Templates.
	renderer, err := views.NewRenderer("./server/views/*.html")
	if err != nil {
		return nil, err
	}
	e.SetRenderer(renderer)

	// Assets.
	e.Static("/app", "server/app")
	e.Static("/modules", "server/app/node_modules")
	e.Static("/assets/css", "server/assets/css")
	e.Static("/assets/js", "server/assets/js")
	e.Static("/assets/img", "server/assets/img")
	e.Static("/assets/fonts", "server/assets/fonts")
	e.Static("/assets/bootstrap", "server/app/node_modules/bootstrap/dist")

	// Middleware
	e.Pre(middleware.AddTrailingSlash())
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CSRF([]byte("secret")))
	e.Use(middleware.Secure())

	// Web
	homeHandler := home.NewHandlerFunc(serverCtx)

	e.GET("/", homeHandler)
	e.GET("/logout/", logout.NewHandlerFunc(serverCtx))

	e.GET("/home/", homeHandler)
	e.GET("/events/", homeHandler)
	e.GET("/eventstream/", homeHandler)
	e.GET("/notifications/", homeHandler)
	e.GET("/profile/", homeHandler)

	facebookCallbackPath, _ := url.Parse("/auth/facebook/callback")
	facebookCallback := serverCtx.CanonicalURL.ResolveReference(facebookCallbackPath).String()
	facebookAuth := facebook.NewAuthenticator(
		cfg.FacebookClientId, cfg.FacebookClientSecret, facebookCallback)
	auth.Bind(serverCtx, e.Group("/auth/facebook"), facebookAuth)

	redditCallbackPath, _ := url.Parse("/auth/reddit/callback")
	redditCallback := serverCtx.CanonicalURL.ResolveReference(redditCallbackPath).String()
	redditAuth := reddit.NewAuthenticator(
		cfg.RedditClientId, cfg.RedditClientSecret, redditCallback, serverCtx.SSLEnabled)
	auth.Bind(serverCtx, e.Group("/auth/reddit"), redditAuth)

	googleCallbackPath, _ := url.Parse("/auth/google/callback")
	googleCallback := serverCtx.CanonicalURL.ResolveReference(googleCallbackPath).String()
	googleAuth := google.NewAuthenticator(
		cfg.GoogleClientId, cfg.GoogleClientSecret, googleCallback)
	auth.Bind(serverCtx, e.Group("/auth/google"), googleAuth)

	githubCallbackPath, _ := url.Parse("/auth/github/callback")
	githubCallback := serverCtx.CanonicalURL.ResolveReference(githubCallbackPath).String()
	githubAuth := github.NewAuthenticator(
		cfg.GitHubClientId, cfg.GitHubClientSecret, githubCallback)
	auth.Bind(serverCtx, e.Group("/auth/github"), githubAuth)

	// Public API
	info.Bind(serverCtx, e.Group("/api/v1/info"))

	// API
	api := e.Group("/api", auth.Required(serverCtx))

	// API - Events
	descendantpublished.Bind(serverCtx, api.Group("/events/descendant.published"))
	db.BindList(serverCtx, api.Group("/events/:kind/:list"))

	// API - Event Stream
	manager := eventstream.NewManager()
	manager.Bind(serverCtx, api.Group("/eventstream"))

	// API - Notifiers
	slack.Bind(serverCtx, api.Group("/notifiers/slack"))
	steemitchat.Bind(serverCtx, api.Group("/notifiers/steemit-chat"))

	// API - Profile
	profile.Bind(serverCtx, api.Group("/profile"))

	// Start server
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start the web server")
	}

	ctx := &Context{
		EventStreamManager: manager,
		listener:           listener,
	}

	ctx.t.Go(func() error {
		e.Run(fasthttp.WithConfig(engine.Config{
			Listener: listener,
		}))
		return nil
	})

	go func() {
		<-ctx.t.Dying()
		listener.Close()
	}()

	return ctx, nil
}

func (ctx *Context) Interrupt() {
	ctx.t.Kill(nil)
}

func (ctx *Context) Wait() error {
	return ctx.t.Wait()
}
