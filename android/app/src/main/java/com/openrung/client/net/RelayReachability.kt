package com.openrung.client.net

import com.openrung.client.model.RelayDescriptor
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.net.InetSocketAddress
import java.net.Socket

object RelayReachability {
    suspend fun checkTcp(relay: RelayDescriptor, timeoutMillis: Int = 5_000): Long {
        return withContext(Dispatchers.IO) {
            val host = relay.publicHost.trim().removePrefix("[").removeSuffix("]")
            val started = System.nanoTime()
            Socket().use { socket ->
                socket.connect(InetSocketAddress(host, relay.publicPort), timeoutMillis)
            }
            (System.nanoTime() - started) / 1_000_000
        }
    }
}
