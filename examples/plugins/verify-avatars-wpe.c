// Verifica a página real em sessão WPE efêmera, sem bridge ou scripts injetados.
#include <wpe/webkit.h>
#include <wpe/headless/wpe-headless.h>
#include <stdio.h>
#include <string.h>
static GMainLoop *loop;
static int result=1;
static void title_changed(GObject *obj,GParamSpec *ps,void *data){
 const char *title=webkit_web_view_get_title(WEBKIT_WEB_VIEW(obj));
 if(title && !strcmp(title,"PAPO_PLAYER_AVATARS_OK")){puts("PASS: WPE sandbox decodificou os dois avatares do jogo");result=0;g_main_loop_quit(loop);}
}
static gboolean timeout_cb(void *data){fputs("FAIL: os avatares não foram confirmados dentro do prazo\n",stderr);g_main_loop_quit(loop);return G_SOURCE_REMOVE;}
int main(void){
 char url[8192];if(!fgets(url,sizeof(url),stdin))return 2;url[strcspn(url,"\r\n")]=0;
 GError *error=NULL;WPEDisplay *display=wpe_display_headless_new();
 if(!wpe_display_connect(display,&error)){fprintf(stderr,"headless: %s\n",error->message);return 2;}
 WebKitNetworkSession *session=webkit_network_session_new_ephemeral();
 WebKitWebView *web=WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW,"display",display,"network-session",session,NULL));
 WPEView *view=webkit_web_view_get_wpe_view(web);wpe_view_resized(view,800,600);wpe_view_set_visible(view,TRUE);
 g_object_set(webkit_web_view_get_settings(web),"enable-write-console-messages-to-stdout",TRUE,NULL);
 loop=g_main_loop_new(NULL,FALSE);g_signal_connect(web,"notify::title",G_CALLBACK(title_changed),NULL);
 g_timeout_add_seconds(20,timeout_cb,NULL);webkit_web_view_load_uri(web,url);g_main_loop_run(loop);
 g_object_unref(web);g_object_unref(session);g_object_unref(display);g_main_loop_unref(loop);return result;
}
