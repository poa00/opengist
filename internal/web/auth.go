package web

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/gitea"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/gitlab"
	"github.com/markbates/goth/providers/openidConnect"
	"github.com/rs/zerolog/log"
	"github.com/thomiceli/opengist/internal/config"
	"github.com/thomiceli/opengist/internal/db"
	"github.com/thomiceli/opengist/internal/utils"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	GitHubProvider = "github"
	GitLabProvider = "gitlab"
	GiteaProvider  = "gitea"
	OpenIDConnect  = "openid-connect"
)

var title = cases.Title(language.English)

func register(ctx echo.Context) error {
	setData(ctx, "title", tr(ctx, "auth.new-account"))
	setData(ctx, "htmlTitle", "New account")
	setData(ctx, "disableForm", getData(ctx, "DisableLoginForm"))
	setData(ctx, "isLoginPage", false)
	return html(ctx, "auth_form.html")
}

func processRegister(ctx echo.Context) error {
	if getData(ctx, "DisableSignup") == true {
		return errorRes(403, "Signing up is disabled", nil)
	}

	if getData(ctx, "DisableLoginForm") == true {
		return errorRes(403, "Signing up via registration form is disabled", nil)
	}

	setData(ctx, "title", "New account")
	setData(ctx, "htmlTitle", "New account")

	sess := getSession(ctx)

	dto := new(db.UserDTO)
	if err := ctx.Bind(dto); err != nil {
		return errorRes(400, "Cannot bind data", err)
	}

	if err := ctx.Validate(dto); err != nil {
		addFlash(ctx, utils.ValidationMessages(&err), "error")
		return html(ctx, "auth_form.html")
	}

	if exists, err := db.UserExists(dto.Username); err != nil || exists {
		addFlash(ctx, "Username already exists", "error")
		return html(ctx, "auth_form.html")
	}

	user := dto.ToUser()

	password, err := utils.Argon2id.Hash(user.Password)
	if err != nil {
		return errorRes(500, "Cannot hash password", err)
	}
	user.Password = password

	if err = user.Create(); err != nil {
		return errorRes(500, "Cannot create user", err)
	}

	if user.ID == 1 {
		if err = user.SetAdmin(); err != nil {
			return errorRes(500, "Cannot set user admin", err)
		}
	}

	sess.Values["user"] = user.ID
	saveSession(sess, ctx)

	return redirect(ctx, "/")
}

func login(ctx echo.Context) error {
	setData(ctx, "title", tr(ctx, "auth.login"))
	setData(ctx, "htmlTitle", "Login")
	setData(ctx, "disableForm", getData(ctx, "DisableLoginForm"))
	setData(ctx, "isLoginPage", true)
	return html(ctx, "auth_form.html")
}

func processLogin(ctx echo.Context) error {
	if getData(ctx, "DisableLoginForm") == true {
		return errorRes(403, "Logging in via login form is disabled", nil)
	}

	var err error
	sess := getSession(ctx)

	dto := &db.UserDTO{}
	if err = ctx.Bind(dto); err != nil {
		return errorRes(400, "Cannot bind data", err)
	}
	password := dto.Password

	var user *db.User

	if user, err = db.GetUserByUsername(dto.Username); err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return errorRes(500, "Cannot get user", err)
		}
		log.Warn().Msg("Invalid HTTP authentication attempt from " + ctx.RealIP())
		addFlash(ctx, "Invalid credentials", "error")
		return redirect(ctx, "/login")
	}

	if ok, err := utils.Argon2id.Verify(password, user.Password); !ok {
		if err != nil {
			return errorRes(500, "Cannot check for password", err)
		}
		log.Warn().Msg("Invalid HTTP authentication attempt from " + ctx.RealIP())
		addFlash(ctx, "Invalid credentials", "error")
		return redirect(ctx, "/login")
	}

	sess.Values["user"] = user.ID
	sess.Options.MaxAge = 60 * 60 * 24 * 365 // 1 year
	saveSession(sess, ctx)
	deleteCsrfCookie(ctx)

	return redirect(ctx, "/")
}

func oauthCallback(ctx echo.Context) error {
	user, err := gothic.CompleteUserAuth(ctx.Response(), ctx.Request())
	if err != nil {
		return errorRes(400, "Cannot complete user auth: "+err.Error(), err)
	}

	currUser := getUserLogged(ctx)
	if currUser != nil {
		// if user is logged in, link account to user and update its avatar URL
		updateUserProviderInfo(currUser, user.Provider, user)

		if err = currUser.Update(); err != nil {
			return errorRes(500, "Cannot update user "+title.String(user.Provider)+" id", err)
		}

		addFlash(ctx, "Account linked to "+title.String(user.Provider), "success")
		return redirect(ctx, "/settings")
	}

	// if user is not in database, create it
	userDB, err := db.GetUserByProvider(user.UserID, user.Provider)
	if err != nil {
		if getData(ctx, "DisableSignup") == true {
			return errorRes(403, "Signing up is disabled", nil)
		}

		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return errorRes(500, "Cannot get user", err)
		}

		userDB = &db.User{
			Username: user.NickName,
			Email:    user.Email,
			MD5Hash:  fmt.Sprintf("%x", md5.Sum([]byte(strings.ToLower(strings.TrimSpace(user.Email))))),
		}

		// set provider id and avatar URL
		updateUserProviderInfo(userDB, user.Provider, user)

		if err = userDB.Create(); err != nil {
			if db.IsUniqueConstraintViolation(err) {
				addFlash(ctx, "Username "+user.NickName+" already exists in Opengist", "error")
				return redirect(ctx, "/login")
			}

			return errorRes(500, "Cannot create user", err)
		}

		if userDB.ID == 1 {
			if err = userDB.SetAdmin(); err != nil {
				return errorRes(500, "Cannot set user admin", err)
			}
		}

		var resp *http.Response
		switch user.Provider {
		case GitHubProvider:
			resp, err = http.Get("https://github.com/" + user.NickName + ".keys")
		case GitLabProvider:
			resp, err = http.Get(urlJoin(config.C.GitlabUrl, user.NickName+".keys"))
		case GiteaProvider:
			resp, err = http.Get(urlJoin(config.C.GiteaUrl, user.NickName+".keys"))
		case OpenIDConnect:
			err = errors.New("cannot get keys from OIDC provider")
		}

		if err == nil {
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				addFlash(ctx, "Could not get user keys", "error")
				log.Error().Err(err).Msg("Could not get user keys")
			}

			keys := strings.Split(string(body), "\n")
			if len(keys[len(keys)-1]) == 0 {
				keys = keys[:len(keys)-1]
			}
			for _, key := range keys {
				sshKey := db.SSHKey{
					Title:   "Added from " + user.Provider,
					Content: key,
					User:    *userDB,
				}

				if err = sshKey.Create(); err != nil {
					addFlash(ctx, "Could not create ssh key", "error")
					log.Error().Err(err).Msg("Could not create ssh key")
				}
			}
		}
	}

	sess := getSession(ctx)
	sess.Values["user"] = userDB.ID
	saveSession(sess, ctx)
	deleteCsrfCookie(ctx)

	return redirect(ctx, "/")
}

func oauth(ctx echo.Context) error {
	provider := ctx.Param("provider")

	httpProtocol := "http"
	if ctx.Request().TLS != nil || ctx.Request().Header.Get("X-Forwarded-Proto") == "https" {
		httpProtocol = "https"
	}

	var opengistUrl string
	if config.C.ExternalUrl != "" {
		opengistUrl = config.C.ExternalUrl
	} else {
		opengistUrl = httpProtocol + "://" + ctx.Request().Host
	}

	switch provider {
	case GitHubProvider:
		goth.UseProviders(
			github.New(
				config.C.GithubClientKey,
				config.C.GithubSecret,
				urlJoin(opengistUrl, "/oauth/github/callback"),
			),
		)

	case GitLabProvider:
		goth.UseProviders(
			gitlab.NewCustomisedURL(
				config.C.GitlabClientKey,
				config.C.GitlabSecret,
				urlJoin(opengistUrl, "/oauth/gitlab/callback"),
				urlJoin(config.C.GitlabUrl, "/oauth/authorize"),
				urlJoin(config.C.GitlabUrl, "/oauth/token"),
				urlJoin(config.C.GitlabUrl, "/api/v4/user"),
			),
		)

	case GiteaProvider:
		goth.UseProviders(
			gitea.NewCustomisedURL(
				config.C.GiteaClientKey,
				config.C.GiteaSecret,
				urlJoin(opengistUrl, "/oauth/gitea/callback"),
				urlJoin(config.C.GiteaUrl, "/login/oauth/authorize"),
				urlJoin(config.C.GiteaUrl, "/login/oauth/access_token"),
				urlJoin(config.C.GiteaUrl, "/api/v1/user"),
			),
		)
	case OpenIDConnect:
		oidcProvider, err := openidConnect.New(
			config.C.OIDCClientKey,
			config.C.OIDCSecret,
			urlJoin(opengistUrl, "/oauth/openid-connect/callback"),
			config.C.OIDCDiscoveryUrl,
			"openid",
			"email",
			"profile",
		)

		if err != nil {
			return errorRes(500, "Cannot create OIDC provider", err)
		}

		goth.UseProviders(oidcProvider)
	}

	currUser := getUserLogged(ctx)
	if currUser != nil {
		// Map each provider to a function that checks the relevant ID in currUser
		providerIDCheckMap := map[string]func() bool{
			GitHubProvider: func() bool { return currUser.GithubID != "" },
			GitLabProvider: func() bool { return currUser.GitlabID != "" },
			GiteaProvider:  func() bool { return currUser.GiteaID != "" },
			OpenIDConnect:  func() bool { return currUser.OIDCID != "" },
		}

		// Check if the provider is valid and if the user has a linked ID
		// Means that the user wants to unlink the account
		if checkFunc, exists := providerIDCheckMap[provider]; exists && checkFunc() {
			if err := currUser.DeleteProviderID(provider); err != nil {
				return errorRes(500, "Cannot unlink account from "+title.String(provider), err)
			}

			addFlash(ctx, "Account unlinked from "+title.String(provider), "success")
			return redirect(ctx, "/settings")
		}
	}

	ctxValue := context.WithValue(ctx.Request().Context(), gothic.ProviderParamKey, provider)
	ctx.SetRequest(ctx.Request().WithContext(ctxValue))
	if provider != GitHubProvider && provider != GitLabProvider && provider != GiteaProvider && provider != OpenIDConnect {
		return errorRes(400, "Unsupported provider", nil)
	}

	gothic.BeginAuthHandler(ctx.Response(), ctx.Request())
	return nil
}

func logout(ctx echo.Context) error {
	deleteSession(ctx)
	deleteCsrfCookie(ctx)
	return redirect(ctx, "/all")
}

func urlJoin(base string, elem ...string) string {
	joined, err := url.JoinPath(base, elem...)
	if err != nil {
		log.Error().Err(err).Msg("Cannot join url")
	}

	return joined
}

func updateUserProviderInfo(userDB *db.User, provider string, user goth.User) {
	userDB.AvatarURL = getAvatarUrlFromProvider(provider, user.UserID)
	switch provider {
	case GitHubProvider:
		userDB.GithubID = user.UserID
	case GitLabProvider:
		userDB.GitlabID = user.UserID
	case GiteaProvider:
		userDB.GiteaID = user.UserID
	case OpenIDConnect:
		userDB.OIDCID = user.UserID
		userDB.AvatarURL = user.AvatarURL
	}
}

func getAvatarUrlFromProvider(provider string, identifier string) string {
	switch provider {
	case GitHubProvider:
		return "https://avatars.githubusercontent.com/u/" + identifier + "?v=4"
	case GitLabProvider:
		return urlJoin(config.C.GitlabUrl, "/uploads/-/system/user/avatar/", identifier, "/avatar.png") + "?width=400"
	case GiteaProvider:
		resp, err := http.Get(urlJoin(config.C.GiteaUrl, "/api/v1/users/", identifier))
		if err != nil {
			log.Error().Err(err).Msg("Cannot get user from Gitea")
			return ""
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Error().Err(err).Msg("Cannot read Gitea response body")
			return ""
		}

		var result map[string]interface{}
		err = json.Unmarshal(body, &result)
		if err != nil {
			log.Error().Err(err).Msg("Cannot unmarshal Gitea response body")
			return ""
		}

		field, ok := result["avatar_url"]
		if !ok {
			log.Error().Msg("Field 'avatar_url' not found in Gitea JSON response")
			return ""
		}
		return field.(string)
	}
	return ""
}
