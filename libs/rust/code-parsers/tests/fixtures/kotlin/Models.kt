package com.example.shop.model

import kotlinx.serialization.Serializable
import java.time.Instant as Timestamp
import com.example.shop.util.*

/**
 * A product in the catalogue.
 * @property id the stable identifier
 */
@Serializable
data class Product(val id: Long, val name: String, val price: Price) : Comparable<Product> {
    override fun compareTo(other: Product): Int = price.compareTo(other.price)
}

@JvmInline
value class Price(val cents: Long) : Comparable<Price> {
    override fun compareTo(other: Price): Int = cents.compareTo(other.cents)
}

/** The result of a checkout. */
sealed class CheckoutResult {
    data class Success(val orderId: String) : CheckoutResult()
    data class Failure(val reason: String, val cause: Throwable? = null) : CheckoutResult()
    object Cancelled : CheckoutResult()
}

enum class Currency(val symbol: String) {
    EUR("€"),
    USD("$");

    fun format(cents: Long): String = "$symbol${cents / 100}"
}

typealias ProductIndex = Map<Long, Product>
typealias Handler<T> = (T) -> Unit

internal abstract class Repository<T : Any, ID>(protected val store: Store) : Closeable, Iterable<T> {
    abstract fun find(id: ID): T?
    open fun all(): List<T> = store.load()
}
