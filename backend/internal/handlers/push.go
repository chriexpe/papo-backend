package handlers

import (
	"errors"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

type registerPushRequest struct {
	Provider string `json:"provider"`
	Address string `json:"address"`
	AuthToken string `json:"auth_token"`
	Platform string `json:"platform"`
	AppVersion *string `json:"app_version"`
}

func RegisterPushHandler(baseURL string, c echo.Context) error {
	userID,ok:=c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID=="" { return utils.SendProblem(c,baseURL,401,"unauthorized","Token inválido ou expirado","token de autenticação ausente, inválido ou expirado") }
	targetID:=c.Param("user_id"); installationID:=c.Param("installation_id")
	var req registerPushRequest
	if err:=c.Bind(&req); err!=nil { return utils.SendProblem(c,baseURL,400,"invalid-param","Parâmetro inválido","corpo da requisição inválido") }
	cookie,err:=c.Cookie("Auth"); if err!=nil || cookie.Value=="" { return utils.SendProblem(c,baseURL,401,"unauthorized","Token inválido ou expirado","sessão ativa obrigatória") }
	connectionID,err:=services.CurrentConnectionID(c.Request().Context(),userID,cookie.Value)
	if err!=nil || connectionID=="" { return utils.SendProblem(c,baseURL,401,"unauthorized","Token inválido ou expirado","sessão ativa obrigatória") }
	info,err:=services.RegisterPush(c.Request().Context(),userID,targetID,connectionID,services.RegisterPushRequest{
		InstallationID:installationID,Provider:req.Provider,Address:req.Address,AuthToken:req.AuthToken,Platform:req.Platform,AppVersion:req.AppVersion,
	})
	switch {
	case errors.Is(err,services.ErrPermissionDenied):
		return utils.SendProblem(c,baseURL,http.StatusForbidden,"forbidden","Acesso negado","somente o próprio usuário pode registrar push")
	case errors.Is(err,services.ErrInvalidInput):
		return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","push registration inválida")
	case err!=nil:
		return utils.SendProblem(c,baseURL,http.StatusInternalServerError,"internal","Erro interno","falha ao registrar push")
	}
	return c.JSON(http.StatusOK,info)
}

func UnregisterPushHandler(baseURL string,c echo.Context) error {
	userID,ok:=c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID=="" { return utils.SendProblem(c,baseURL,401,"unauthorized","Token inválido ou expirado","token de autenticação ausente, inválido ou expirado") }
	err:=services.UnregisterPush(c.Request().Context(),userID,c.Param("user_id"),c.Param("installation_id"))
	switch {
	case errors.Is(err,services.ErrPermissionDenied):
		return utils.SendProblem(c,baseURL,403,"forbidden","Acesso negado","somente o próprio usuário pode remover push")
	case errors.Is(err,services.ErrInvalidInput):
		return utils.SendProblem(c,baseURL,400,"invalid-param","Parâmetro inválido","installation_id inválido")
	case err!=nil:
		return utils.SendProblem(c,baseURL,500,"internal","Erro interno","falha ao remover push")
	}
	return c.NoContent(http.StatusNoContent)
}
