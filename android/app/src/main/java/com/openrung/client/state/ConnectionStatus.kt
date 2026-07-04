package com.openrung.client.state

import androidx.annotation.StringRes
import com.openrung.client.R
import com.openrung.client.model.ExitNodeRegion
import com.openrung.client.model.RecentNode

enum class ConnectionStatus(@StringRes val labelResId: Int) {
    DISCONNECTED(R.string.status_disconnected),
    PREPARING(R.string.status_preparing),
    CONNECTING(R.string.status_connecting),
    CONNECTED(R.string.status_connected),
    DISCONNECTING(R.string.status_disconnecting),
    FAILED(R.string.status_failed),
}

/** Load state of the exit-node map directory (the list of available exit-node regions). */
enum class DirectoryStatus {
    IDLE,
    LOADING,
    LOADED,
    FAILED,
}

data class OpenRungUiState(
    val status: ConnectionStatus = ConnectionStatus.DISCONNECTED,
    val brokerUrl: String = "",
    val relayLabel: String? = null,
    val lastError: String? = null,
    val logLines: List<String> = emptyList(),
    val availableRegions: List<ExitNodeRegion> = emptyList(),
    val recentRegions: List<RecentNode> = emptyList(),
    val directoryStatus: DirectoryStatus = DirectoryStatus.IDLE,
) {
    val isWorking: Boolean
        get() = status == ConnectionStatus.PREPARING ||
            status == ConnectionStatus.CONNECTING ||
            status == ConnectionStatus.DISCONNECTING

    val isConnected: Boolean
        get() = status == ConnectionStatus.CONNECTED
}
