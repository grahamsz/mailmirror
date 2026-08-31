package app.rolltop.mobile

import android.Manifest
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.Color
import android.net.Uri
import android.net.http.SslError
import android.os.Build
import android.os.Bundle
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.webkit.CookieManager
import android.webkit.SslErrorHandler
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.view.ViewCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.lifecycle.Lifecycle
import androidx.webkit.ServiceWorkerClientCompat
import androidx.webkit.ServiceWorkerControllerCompat
import androidx.webkit.WebViewFeature

class MainActivity : ComponentActivity() {
    private var webView: WebView? = null
    private var androidWebBridge: AndroidWebBridge? = null
    private var rolltopWebChromeClient: RolltopWebChromeClient? = null
    private var loadingOverlay: FrameLayout? = null
    private var loadingAnimationView: RolltopLoadingView? = null
    private var loadingRevealGate: LoadingRevealGate? = null
    // A notification can open a message in an otherwise empty WebView history.
    // Remember its in-app destination so Android Back does not follow WebView's
    // initial history entry out of the app.
    private var notificationMessageBackUrl: String? = null
    private val nativeShareStore by lazy { NativeShareStore(applicationContext) }
    private var updatePromptPolicy = UpdatePromptPolicy()
    private val contactPermissionLauncher = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        androidWebBridge?.handleContactPermissionResult(granted)
    }
    private val notificationPermissionLauncher = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) NativePushRegistration.maybeRegister(this)
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        updatePromptPolicy = UpdatePromptPolicy(savedInstanceState?.getInt(STATE_PROMPTED_UPDATE_CODE) ?: 0)
        installBackNavigation()
        WindowCompat.setDecorFitsSystemWindows(window, false)
        WindowInsetsControllerCompat(window, window.decorView).apply {
            isAppearanceLightStatusBars = true
            isAppearanceLightNavigationBars = true
        }
        NotificationChannels.ensure(this)
        NewMailPollWorker.schedule(this)
        UpdateCheckWorker.schedule(this)
        requestNotificationPermission()
        if (RolltopPrefs.serverUrl(this).isBlank()) {
            showSetup()
        } else {
            showWeb(
                intent,
                savedInstanceState?.getBundle(STATE_WEB_VIEW),
                savedInstanceState?.getLong(STATE_LOADING_ANIMATION_ELAPSED_MS) ?: 0L
            )
        }
    }

    override fun onStart() {
        super.onStart()
        checkForServerUpdate()
        NativePushRegistration.maybeRegister(this)
        PushSubscriptionWorker.schedule(this)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (webView == null) {
            if (RolltopPrefs.serverUrl(this).isBlank()) showSetup() else showWeb(intent)
        } else {
            explicitUrlForIntent(intent)?.let { target ->
                notificationMessageBackUrl = AndroidBackNavigation.messageFallbackUrl(
                    RolltopPrefs.serverUrl(this),
                    target
                )
                webView?.loadUrl(target)
                consumeNavigationIntent()
            }
        }
    }

    override fun onPause() {
        rememberCurrentLocation()
        CookieManager.getInstance().flush()
        super.onPause()
    }

    override fun onSaveInstanceState(outState: Bundle) {
        rememberCurrentLocation()
        outState.putInt(STATE_PROMPTED_UPDATE_CODE, updatePromptPolicy.lastPromptedVersionCode)
        outState.putLong(
            STATE_LOADING_ANIMATION_ELAPSED_MS,
            loadingAnimationView?.elapsedMs ?: RolltopLoadingView.DURATION_MS
        )
        webView?.let { view ->
            val state = Bundle()
            view.saveState(state)
            outState.putBundle(STATE_WEB_VIEW, state)
        }
        super.onSaveInstanceState(outState)
    }

    override fun onDestroy() {
        cancelLoadingExperience()
        androidWebBridge?.close()
        androidWebBridge = null
        rolltopWebChromeClient?.cancelPendingRequest()
        rolltopWebChromeClient = null
        webView?.destroy()
        webView = null
        super.onDestroy()
    }

    @Deprecated("Android contact picking uses the platform activity result callback.")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (androidWebBridge?.handleActivityResult(requestCode, resultCode, data) == true) return
        if (rolltopWebChromeClient?.handleActivityResult(requestCode, resultCode, data) == true) return
        super.onActivityResult(requestCode, resultCode, data)
    }

    private fun showSetup(initialUrl: String = RolltopPrefs.serverUrl(this), message: String = "") {
        cancelLoadingExperience()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER
            setBackgroundColor(SHELL_BACKGROUND)
            setPadding(dp(24), dp(24), dp(24), dp(24))
            layoutParams = LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT)
        }
        val title = TextView(this).apply {
            text = "Rolltop"
            textSize = 28f
        }
        val feedback = TextView(this).apply {
            text = message
            setTextColor(Color.rgb(151, 43, 43))
            visibility = if (message.isBlank()) View.GONE else View.VISIBLE
        }
        val input = EditText(this).apply {
            hint = "https://mail.example.com"
            inputType = android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_URI
            setSingleLine(true)
            setText(initialUrl)
            setSelection(text.length)
        }
        val connect = Button(this).apply { text = "Connect" }
        connect.setOnClickListener {
            val normalized = RolltopPrefs.normalizeBaseUrl(input.text.toString())
            if (normalized.isEmpty()) {
                input.error = "Enter a valid HTTPS Rolltop server URL."
                input.requestFocus()
                return@setOnClickListener
            }

            input.isEnabled = false
            connect.isEnabled = false
            connect.text = "Checking..."
            feedback.setTextColor(Color.rgb(74, 81, 78))
            feedback.text = "Checking the Rolltop server..."
            feedback.visibility = View.VISIBLE

            Thread({
                val result = RolltopServerValidator.validate(normalized)
                runOnUiThread {
                    if (isFinishing || isDestroyed) return@runOnUiThread
                    input.isEnabled = true
                    connect.isEnabled = true
                    connect.text = "Connect"
                    if (!result.valid) {
                        feedback.setTextColor(Color.rgb(151, 43, 43))
                        feedback.text = result.message
                        return@runOnUiThread
                    }
                    RolltopPrefs.setServerUrl(this@MainActivity, normalized)
                    showWeb(intent)
                }
            }, "rolltop-server-check").start()
        }
        val role = Button(this).apply {
            text = "Set as default mail app"
            setOnClickListener { RoleHelper.requestDefaultMailRole(this@MainActivity) }
        }
        root.addView(title, spacedLayoutParams(top = 0, bottom = 12))
        root.addView(feedback, spacedLayoutParams(bottom = 12))
        root.addView(input, spacedLayoutParams(bottom = 8))
        root.addView(connect, spacedLayoutParams(bottom = 4))
        root.addView(role, spacedLayoutParams())
        applySystemBarInsets(root, dp(24))
        setContentView(root)
    }

    private fun showWeb(
        sourceIntent: Intent?,
        restoredState: Bundle? = null,
        restoredLoadingElapsedMs: Long = 0L
    ) {
        cancelLoadingExperience()
        androidWebBridge?.close()
        androidWebBridge = null
        val view = WebView(this)
        view.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO_HIDE_DESCENDANTS
        webView = view
        val serverOrigin = RolltopPrefs.serverUrl(this)
        val loadingAnimation = RolltopLoadingView(this).apply {
            importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
        }
        val loadingOverlay = FrameLayout(this).apply {
            setBackgroundColor(SHELL_BACKGROUND)
            isClickable = true
            importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_YES
            contentDescription = getString(R.string.app_name)
            addView(
                loadingAnimation,
                FrameLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT,
                    ViewGroup.LayoutParams.MATCH_PARENT
                )
            )
        }
        fun removeLoadingOverlay() {
            (view.parent as? View)?.setBackgroundColor(Color.WHITE)
            (loadingOverlay.parent as? ViewGroup)?.removeView(loadingOverlay)
            if (this.loadingOverlay === loadingOverlay) this.loadingOverlay = null
            if (loadingAnimationView === loadingAnimation) loadingAnimationView = null
        }
        lateinit var revealGate: LoadingRevealGate
        revealGate = LoadingRevealGate {
            if (webView !== view || loadingRevealGate !== revealGate || loadingOverlay.parent == null) {
                return@LoadingRevealGate
            }
            loadingRevealGate = null
            loadingOverlay.isClickable = false
            loadingOverlay.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO_HIDE_DESCENDANTS
            view.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_AUTO
            if (!loadingAnimation.animationsEnabled) {
                removeLoadingOverlay()
            } else {
                loadingOverlay.animate()
                    .alpha(0f)
                    .setDuration(LOADING_CROSSFADE_MS)
                    .withEndAction(::removeLoadingOverlay)
                    .start()
            }
        }
        loadingAnimation.onAnimationComplete = revealGate::markAnimationReady
        installNativeShareServiceWorkerInterceptor(serverOrigin)
        CookieManager.getInstance().setAcceptCookie(true)
        CookieManager.getInstance().setAcceptThirdPartyCookies(view, true)
        view.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            databaseEnabled = true
            cacheMode = WebSettings.LOAD_DEFAULT
            mediaPlaybackRequiresUserGesture = true
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            setSupportMultipleWindows(true)
        }
        view.webViewClient = object : WebViewClient() {
            private var mainFrameCommitted = false
            private var failureHandled = false
            private var mainNavigationGeneration = 0L

            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                val url = request.url.toString()
                val context = WebNavigationContext(
                    isMainFrame = request.isForMainFrame,
                    hasUserGesture = request.hasGesture(),
                    isRedirect = request.isRedirect
                )
                val action = WebNavigationPolicy.action(serverOrigin, url, context)
                val handled = handleWebNavigation(url, context, action)
                if (action == WebNavigationAction.BLOCK && request.isForMainFrame &&
                    (request.isRedirect || !request.hasGesture())
                ) {
                    failInitialLoad(view, "The server redirected outside the configured Rolltop address.")
                }
                return handled
            }

            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? =
                nativeShareStore.intercept(request, serverOrigin) ?: super.shouldInterceptRequest(view, request)

            override fun onPageStarted(view: WebView, url: String, favicon: Bitmap?) {
                mainNavigationGeneration += 1
                if (webView === view && loadingRevealGate === revealGate) {
                    revealGate.markContentPending()
                }
            }

            override fun onPageCommitVisible(view: WebView, url: String) {
                mainFrameCommitted = true
                RolltopPrefs.rememberVisitedUrl(this@MainActivity, url)
            }

            override fun onPageFinished(view: WebView, url: String) {
                RolltopPrefs.rememberVisitedUrl(this@MainActivity, url)
                NativePushRegistration.maybeRegister(this@MainActivity)
                PushSubscriptionWorker.schedule(this@MainActivity)
                if (RolltopPrefs.internalLocation(serverOrigin, url) != null) {
                    val expectedGeneration = mainNavigationGeneration
                    view.postVisualStateCallback(System.nanoTime(), object : WebView.VisualStateCallback() {
                        override fun onComplete(requestId: Long) {
                            if (webView !== view || loadingRevealGate !== revealGate ||
                                mainNavigationGeneration != expectedGeneration ||
                                RolltopPrefs.internalLocation(serverOrigin, view.url.orEmpty()) == null
                            ) return
                            revealGate.markContentReady()
                        }
                    })
                }
            }

            override fun doUpdateVisitedHistory(view: WebView, url: String, isReload: Boolean) {
                RolltopPrefs.rememberVisitedUrl(this@MainActivity, url)
            }

            override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
                if (request.isForMainFrame) {
                    failInitialLoad(view, "Could not connect to this Rolltop server. Check the address and try again.")
                }
            }

            override fun onReceivedHttpError(view: WebView, request: WebResourceRequest, errorResponse: WebResourceResponse) {
                if (request.isForMainFrame && errorResponse.statusCode >= 400) {
                    failInitialLoad(view, "The server did not recognize Rolltop at this address. Check the URL and try again.")
                }
            }

            override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: SslError) {
                handler.cancel()
                failInitialLoad(view, "Rolltop could not verify this server's HTTPS certificate. Check the URL and certificate, then try again.")
            }

            private fun failInitialLoad(view: WebView, message: String) {
                if (mainFrameCommitted || failureHandled) return
                failureHandled = true
                view.post { showConnectionFailure(view, message) }
            }
        }
        rolltopWebChromeClient = RolltopWebChromeClient(this) { navigation ->
            handleWebNavigation(
                navigation.url,
                WebNavigationContext(
                    isMainFrame = true,
                    hasUserGesture = navigation.hasUserGesture,
                    isRedirect = navigation.isRedirect,
                    isNewWindow = true
                )
            )
        }.also { view.webChromeClient = it }
        androidWebBridge = AndroidWebBridge(this, view, serverOrigin, nativeShareStore) {
            contactPermissionLauncher.launch(Manifest.permission.READ_CONTACTS)
        }.also { it.attach() }
        val root = FrameLayout(this).apply {
            setBackgroundColor(SHELL_BACKGROUND)
            addView(view, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
            addView(loadingOverlay, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        }
        this.loadingOverlay = loadingOverlay
        loadingAnimationView = loadingAnimation
        loadingRevealGate = revealGate
        applySystemBarInsets(root)
        setContentView(root)
        if (lifecycle.currentState.isAtLeast(Lifecycle.State.STARTED)) checkForServerUpdate()

        val explicitTarget = explicitUrlForIntent(sourceIntent)
        notificationMessageBackUrl = explicitTarget?.let {
            AndroidBackNavigation.messageFallbackUrl(RolltopPrefs.serverUrl(this), it)
        }
        val restored = explicitTarget == null && restoredState != null && view.restoreState(restoredState) != null
        if (restored) view.reload() else view.loadUrl(explicitTarget ?: urlForIntent(sourceIntent))
        loadingAnimation.start(restoredLoadingElapsedMs)
        if (explicitTarget != null) consumeNavigationIntent()
    }

    private fun urlForIntent(sourceIntent: Intent?): String {
        return explicitUrlForIntent(sourceIntent)
            ?: RolltopPrefs.lastVisitedUrl(this).takeIf { it.isNotBlank() }
            ?: RolltopPrefs.buildUrl(this, "/mail")
    }

    private fun explicitUrlForIntent(sourceIntent: Intent?): String? {
        if (sourceIntent?.hasExtra(NotificationIntentPolicy.EXTRA_TARGET_PATH) == true) {
            val expectedServer = sourceIntent.getStringExtra(
                NotificationIntentPolicy.EXTRA_EXPECTED_SERVER_URL
            ).orEmpty()
            val expectedUserID = sourceIntent.getLongExtra(
                NotificationIntentPolicy.EXTRA_EXPECTED_USER_ID,
                0
            )
            val currentUserID = RolltopPrefs.newMailCursor(this)?.userId ?: 0
            if (!NotificationIntentPolicy.contextMatches(
                    expectedServer,
                    expectedUserID,
                    RolltopPrefs.serverUrl(this),
                    currentUserID
                )
            ) return null
            return RolltopPrefs.resolveInternalUrl(
                this,
                sourceIntent.getStringExtra(NotificationIntentPolicy.EXTRA_TARGET_PATH)
            )
        }

        return when (sourceIntent?.action) {
            Intent.ACTION_SENDTO, Intent.ACTION_VIEW -> {
                val data = sourceIntent.data
                if (data?.scheme == "mailto") composeUrlFromMailto(data) else null
            }
            Intent.ACTION_SEND, Intent.ACTION_SEND_MULTIPLE -> composeUrlFromShare(sourceIntent)
            else -> null
        }
    }

    private fun showConnectionFailure(failedView: WebView, message: String) {
        if (webView !== failedView || isFinishing || isDestroyed) return
        cancelLoadingExperience()
        androidWebBridge?.close()
        androidWebBridge = null
        rolltopWebChromeClient?.cancelPendingRequest()
        rolltopWebChromeClient = null
        webView = null
        failedView.stopLoading()
        showSetup(RolltopPrefs.serverUrl(this), message)
        failedView.destroy()
    }

    private fun cancelLoadingExperience() {
        loadingRevealGate?.cancel()
        loadingRevealGate = null
        loadingAnimationView?.cancel()
        loadingAnimationView = null
        loadingOverlay?.animate()?.cancel()
        (loadingOverlay?.parent as? ViewGroup)?.removeView(loadingOverlay)
        loadingOverlay = null
    }

    private fun rememberCurrentLocation() {
        RolltopPrefs.rememberVisitedUrl(this, webView?.url)
    }

    private fun handleWebNavigation(
        candidate: String,
        context: WebNavigationContext,
        action: WebNavigationAction = WebNavigationPolicy.action(RolltopPrefs.serverUrl(this), candidate, context)
    ): Boolean {
        return when (action) {
            WebNavigationAction.ALLOW_IN_WEBVIEW -> false
            WebNavigationAction.OPEN_INTERNAL -> {
                webView?.loadUrl(candidate)
                true
            }
            WebNavigationAction.COMPOSE_MAILTO -> {
                webView?.loadUrl(composeUrlFromMailto(Uri.parse(candidate)))
                true
            }
            WebNavigationAction.OPEN_EXTERNAL -> {
                openExternalWebUrl(candidate)
                true
            }
            WebNavigationAction.BLOCK -> {
                if (context.isMainFrame && context.hasUserGesture && !context.isRedirect) {
                    Toast.makeText(this, "Rolltop blocked an unsupported link.", Toast.LENGTH_SHORT).show()
                }
                true
            }
        }
    }

    private fun openExternalWebUrl(candidate: String) {
        val uri = Uri.parse(candidate)
        if (uri.scheme?.lowercase() !in setOf("http", "https")) return
        try {
            startActivity(Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE))
        } catch (_: ActivityNotFoundException) {
            Toast.makeText(this, "No browser can open this link.", Toast.LENGTH_SHORT).show()
        }
    }

    private fun checkForServerUpdate() {
        if (RolltopPrefs.serverUrl(this).isBlank()) return
        UpdateChecker.checkInForeground(this, force = true, shouldPrompt = updatePromptPolicy::shouldPrompt)
    }

    private fun installNativeShareServiceWorkerInterceptor(serverOrigin: String) {
        if (!WebViewFeature.isFeatureSupported(WebViewFeature.SERVICE_WORKER_BASIC_USAGE) ||
            !WebViewFeature.isFeatureSupported(WebViewFeature.SERVICE_WORKER_SHOULD_INTERCEPT_REQUEST)
        ) return
        // Fetches owned by an active service worker bypass the page WebViewClient.
        ServiceWorkerControllerCompat.getInstance().setServiceWorkerClient(object : ServiceWorkerClientCompat() {
            override fun shouldInterceptRequest(request: WebResourceRequest): WebResourceResponse? =
                nativeShareStore.intercept(request, serverOrigin)
        })
    }

    private fun installBackNavigation() {
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                val view = webView
                notificationMessageBackUrl
                    ?.takeIf {
                        AndroidBackNavigation.messageFallbackUrl(
                            RolltopPrefs.serverUrl(this@MainActivity),
                            view?.url
                        ) != null
                    }
                    ?.let {
                        notificationMessageBackUrl = null
                        view?.loadUrl(it)
                        return
                    }
                notificationMessageBackUrl = null
                if (view?.canGoBack() == true) {
                    view.goBack()
                    return
                }
                AndroidBackNavigation.messageFallbackUrl(RolltopPrefs.serverUrl(this@MainActivity), view?.url)
                    ?.let {
                        view?.loadUrl(it)
                        return
                    }

                isEnabled = false
                onBackPressedDispatcher.onBackPressed()
            }
        })
    }

    private fun consumeNavigationIntent() {
        setIntent(Intent(this, MainActivity::class.java).setAction(Intent.ACTION_MAIN))
    }

    private fun applySystemBarInsets(view: View, contentPadding: Int = 0) {
        ViewCompat.setOnApplyWindowInsetsListener(view) { target, insets ->
            val safe = insets.getInsets(WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.displayCutout())
            val keyboard = insets.getInsets(WindowInsetsCompat.Type.ime())
            target.setPadding(
                contentPadding + safe.left,
                contentPadding + safe.top,
                contentPadding + safe.right,
                contentPadding + maxOf(safe.bottom, keyboard.bottom)
            )
            insets
        }
        ViewCompat.requestApplyInsets(view)
    }

    private fun spacedLayoutParams(top: Int = 0, bottom: Int = 0) =
        LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT).apply {
            topMargin = dp(top)
            bottomMargin = dp(bottom)
        }

    private fun dp(value: Int): Int = (value * resources.displayMetrics.density).toInt()

    private fun composeUrlFromMailto(uri: Uri): String {
        val to = uri.schemeSpecificPart.substringBefore('?')
        val subject = uri.getQueryParameter("subject").orEmpty()
        val body = uri.getQueryParameter("body").orEmpty()
        return composeUrl(to = to, subject = subject, body = body)
    }

    private fun composeUrlFromShare(intent: Intent): String {
        val subject = intent.getStringExtra(Intent.EXTRA_SUBJECT).orEmpty()
        val text = intent.getStringExtra(Intent.EXTRA_TEXT).orEmpty()
        return composeUrl(subject = subject, body = text, nativeShareID = nativeShareStore.capture(intent).orEmpty())
    }

    private fun composeUrl(
        to: String = "",
        subject: String = "",
        body: String = "",
        nativeShareID: String = ""
    ): String {
        val builder = Uri.parse(RolltopPrefs.buildUrl(this, "/compose")).buildUpon()
        if (to.isNotBlank()) builder.appendQueryParameter("to", to)
        if (subject.isNotBlank()) builder.appendQueryParameter("subject", subject)
        if (body.isNotBlank()) builder.appendQueryParameter("body", body)
        if (nativeShareID.isNotBlank()) builder.appendQueryParameter("android_share", nativeShareID)
        return builder.build().toString()
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    companion object {
        private const val STATE_WEB_VIEW = "rolltop.web_view_state"
        private const val STATE_PROMPTED_UPDATE_CODE = "rolltop.prompted_update_code"
        private const val STATE_LOADING_ANIMATION_ELAPSED_MS = "rolltop.loading_animation_elapsed_ms"
        private const val LOADING_CROSSFADE_MS = 160L
        private val SHELL_BACKGROUND = Color.rgb(242, 240, 235)
    }
}
