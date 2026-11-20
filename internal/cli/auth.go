package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/auth"
)

type identity struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Handle string `json:"handle,omitempty"`
}

type authResult struct {
	auth.Status
	Channel *identity `json:"channel,omitempty"`
}

func (r *runtime) authCommand(a args) (result, error) {
	ctx := r.opts.Context
	if a.command != "auth set-api-key" && len(a.pos) != 0 {
		return result{}, errors.New("此命令不接收位置参数")
	}
	switch a.command {
	case "auth import":
		if a.value("client") == "" {
			return result{}, errors.New("需要 --client <Google Desktop JSON>")
		}
		if a.has("token") && a.value("token") == "" {
			return result{}, errors.New("--token 需要文件路径")
		}
		if err := r.auth.Import(ctx, a.value("client"), a.value("token"), a.enabled("force")); err != nil {
			return result{}, err
		}
		return result{data: map[string]bool{"imported": true}, text: "已导入 OAuth 配置。\n"}, nil

	case "auth set-api-key":
		fromStdin := a.enabled("stdin")
		if fromStdin && len(a.pos) != 0 || !fromStdin && len(a.pos) != 1 {
			return result{}, errors.New("请提供一个 API Key，或单用 --stdin 读取")
		}
		var key string
		if fromStdin {
			const maxInput = 1 << 20
			data, err := io.ReadAll(io.LimitReader(r.opts.In, maxInput+1))
			if err != nil || len(data) > maxInput {
				return result{}, errors.New("无法从标准输入读取 API Key")
			}
			key = string(data)
		} else {
			key = a.pos[0]
		}
		if err := r.auth.SetAPIKey(ctx, key); err != nil {
			return result{}, err
		}
		return result{data: map[string]bool{"apiKeyConfigured": true}, text: "已配置用于公开读取的 API Key。\n"}, nil

	case "auth login":
		user, err := a.intOption("authuser", 0)
		if err != nil {
			return result{}, err
		}
		timeout, err := a.intOption("timeout", 1)
		if err != nil {
			return result{}, err
		}
		wait := 300 * time.Second
		if timeout != nil {
			if int64(*timeout) > math.MaxInt64/int64(time.Second) {
				return result{}, errors.New("--timeout 数值过大")
			}
			wait = time.Duration(*timeout) * time.Second
		}
		loginCtx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		err = r.auth.Login(loginCtx, auth.LoginOptions{
			AuthUser: user,
			Ready: func(localURL string) error {
				if _, err := fmt.Fprintln(r.opts.Err, "在浏览器中打开 "+localURL+" 完成 Google OAuth 授权"); err != nil {
					return errors.New("无法显示本地登录地址")
				}
				return nil
			},
		})
		if err != nil {
			return result{}, err
		}
		status, err := r.auth.Status(r.key)
		if err != nil {
			return result{}, err
		}
		return renderAuth(authResult{Status: status}, "已保存 OAuth 令牌。\n"), nil

	case "auth logout":
		if err := r.auth.Logout(ctx, a.enabled("all")); err != nil {
			return result{}, err
		}
		return result{data: map[string]bool{"loggedOut": true}, text: "已删除本地登录令牌。\n"}, nil

	default:
		status, err := r.auth.Status(r.key)
		report := authResult{Status: status}
		if err != nil {
			return renderAuth(report, ""), err
		}
		if status.TokenConfigured || a.command == "whoami" {
			channel, err := r.api(nil).GetMyChannel(ctx)
			if err != nil {
				return renderAuth(report, ""), err
			}
			report.Channel = &identity{ID: channel.ID, Title: channel.Snippet.Title}
			if strings.HasPrefix(channel.Snippet.CustomURL, "@") {
				report.Channel.Handle = channel.Snippet.CustomURL
			}
			// A verification request may have refreshed the stored access token.
			newStatus, err := r.auth.Status(r.key)
			if err != nil {
				return renderAuth(report, ""), err
			}
			report.Status = newStatus
		}
		return renderAuth(report, ""), nil
	}
}

func renderAuth(a authResult, prefix string) result {
	var text strings.Builder
	text.WriteString(prefix)
	fmt.Fprintf(&text, "OAuth 客户端 %t，OAuth 令牌 %t（可刷新 %t），API Key %t\n",
		a.ClientConfigured, a.TokenConfigured, a.RefreshTokenPresent, a.APIKeyConfigured)
	if a.AccessTokenExpiresAt != 0 {
		fmt.Fprintf(&text, "访问令牌有效期至 %s\n", time.Unix(a.AccessTokenExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if a.Channel != nil {
		fmt.Fprintf(&text, "频道 %s  %s", a.Channel.ID, oneLine(a.Channel.Title))
		if a.Channel.Handle != "" {
			fmt.Fprint(&text, "  "+oneLine(a.Channel.Handle))
		}
		text.WriteByte('\n')
	}
	for _, warning := range a.Warnings {
		text.WriteString(warning + "\n")
	}
	return result{data: a, text: text.String()}
}
