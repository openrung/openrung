package com.openrung.client.telemetry

import android.content.Context
import java.util.UUID

object ClientIdentity {
    private const val PREFS = "openrung_identity"
    private const val KEY_CLIENT_ID = "client_id"

    fun getOrCreate(context: Context): String {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        prefs.getString(KEY_CLIENT_ID, null)?.takeIf { it.isNotBlank() }?.let { return it }
        return UUID.randomUUID().toString().also {
            prefs.edit().putString(KEY_CLIENT_ID, it).commit()
        }
    }
}
